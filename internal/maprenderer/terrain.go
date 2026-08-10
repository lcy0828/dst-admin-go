package maprenderer

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"sort"
)

const (
	maxImageDimension = 8192
	defaultTileScale  = 8
	noiseWorldRepeat  = 64.0
)

type terrainLayer struct {
	definition TileDefinition
	ids        map[uint16]bool
	texture    *image.NRGBA
}

type edgeAtlas struct {
	texture  *image.NRGBA
	elements map[int]image.Rectangle
}

func pixelsPerTile(width, height int) int {
	scale := defaultTileScale
	for scale > 1 && (width*scale > maxImageDimension || height*scale > maxImageDimension) {
		scale--
	}
	return scale
}

func RenderTerrain(output io.Writer, parsed ParsedSave, assets *AssetSource, definitions []TileDefinition) (int, error) {
	scale := pixelsPerTile(parsed.TileWidth, parsed.TileHeight)
	canvas := image.NewNRGBA(image.Rect(0, 0, parsed.TileWidth*scale, parsed.TileHeight*scale))
	resolvedDefinitions := make(map[string]TileDefinition, len(definitions))
	staticNames := make(map[uint16]string)
	for _, definition := range definitions {
		resolvedDefinitions[definition.Name] = definition
		if definition.StaticID != 0 {
			staticNames[definition.StaticID] = definition.Name
		}
	}
	resolvedNames := make([]string, len(parsed.TileIDs))
	unknown := 0
	for index, id := range parsed.TileIDs {
		name := parsed.TileNames[id]
		if name == "" {
			name = staticNames[id]
		}
		resolvedNames[index] = name
		definition, known := resolvedDefinitions[name]
		if !known {
			unknown++
			fillTile(canvas, index%parsed.TileWidth, index/parsed.TileWidth, parsed.TileHeight, scale, unknownTerrainColor(id))
		} else if definition.Noise == "" {
			fillTile(canvas, index%parsed.TileWidth, index/parsed.TileWidth, parsed.TileHeight, scale, color.NRGBA{R: 27, G: 29, B: 26, A: 255})
		}
	}
	edges, err := loadEdgeAtlas(assets)
	if err != nil {
		return 0, err
	}
	layers, warnings := loadTerrainLayers(assets, definitions, parsed, resolvedNames)
	if len(warnings) > 0 {
		return 0, fmt.Errorf("load official minimap textures: %v", warnings[0])
	}
	for _, layer := range layers {
		renderTerrainLayer(canvas, parsed, scale, resolvedNames, layer, edges)
	}
	if err := renderRoads(canvas, parsed, scale, assets); err != nil {
		return 0, err
	}
	if err := png.Encode(output, canvas); err != nil {
		return 0, fmt.Errorf("encode terrain PNG: %w", err)
	}
	return unknown, nil
}

func loadEdgeAtlas(assets *AssetSource) (edgeAtlas, error) {
	textureData, err := assets.Read("levels/tiles/map_edge.tex")
	if err != nil {
		return edgeAtlas{}, err
	}
	texture, err := DecodeKTEX(textureData)
	if err != nil {
		return edgeAtlas{}, fmt.Errorf("decode map edge texture: %w", err)
	}
	atlasData, err := assets.Read("levels/tiles/map_edge.xml")
	if err != nil {
		return edgeAtlas{}, err
	}
	atlas, err := ParseAtlas(atlasData)
	if err != nil {
		return edgeAtlas{}, err
	}
	elements := make(map[int]image.Rectangle, 48)
	for number := 1; number <= 48; number++ {
		name := fmt.Sprintf("%02d", number)
		element, ok := atlas.Elements[name]
		if !ok {
			return edgeAtlas{}, fmt.Errorf("map edge atlas is missing element %s", name)
		}
		bounds, err := element.Bounds(texture)
		if err != nil {
			return edgeAtlas{}, err
		}
		elements[number] = bounds
	}
	return edgeAtlas{texture: texture, elements: elements}, nil
}

func loadTerrainLayers(assets *AssetSource, definitions []TileDefinition, parsed ParsedSave, resolvedNames []string) ([]terrainLayer, []error) {
	present := make(map[string]map[uint16]bool)
	for index, name := range resolvedNames {
		if name == "" {
			continue
		}
		if present[name] == nil {
			present[name] = make(map[uint16]bool)
		}
		present[name][parsed.TileIDs[index]] = true
	}
	layers := make([]terrainLayer, 0, len(present))
	errorsFound := make([]error, 0)
	for _, definition := range definitions {
		ids := present[definition.Name]
		if len(ids) == 0 || definition.Noise == "" {
			continue
		}
		data, err := assets.ReadLevelTexture(definition.Noise)
		if err != nil {
			errorsFound = append(errorsFound, err)
			continue
		}
		texture, err := DecodeKTEX(data)
		if err != nil {
			errorsFound = append(errorsFound, fmt.Errorf("decode terrain texture %s: %w", definition.Noise, err))
			continue
		}
		layers = append(layers, terrainLayer{definition: definition, ids: ids, texture: texture})
	}
	sort.SliceStable(layers, func(left, right int) bool {
		return layers[left].definition.RenderOrder < layers[right].definition.RenderOrder
	})
	return layers, errorsFound
}

func renderTerrainLayer(canvas *image.NRGBA, parsed ParsedSave, scale int, names []string, layer terrainLayer, edges edgeAtlas) {
	for tileY := 0; tileY < parsed.TileHeight; tileY++ {
		for tileX := 0; tileX < parsed.TileWidth; tileX++ {
			edgeNumber := terrainEdgeNumber(names, parsed.TileWidth, parsed.TileHeight, tileX, tileY, layer.definition.Name)
			if edgeNumber == 0 {
				continue
			}
			renderTerrainTile(canvas, parsed.TileHeight, tileX, tileY, scale, layer, edges.texture, edges.elements[edgeNumber])
		}
	}
}

func terrainEdgeNumber(names []string, width, height, x, y int, wanted string) int {
	if tileNameAt(names, width, height, x, y) == wanted {
		return 1
	}
	cardinal := 0
	if tileNameAt(names, width, height, x-1, y) == wanted {
		cardinal |= 1
	}
	if tileNameAt(names, width, height, x, y-1) == wanted {
		cardinal |= 2
	}
	if tileNameAt(names, width, height, x+1, y) == wanted {
		cardinal |= 4
	}
	if tileNameAt(names, width, height, x, y+1) == wanted {
		cardinal |= 8
	}
	if cardinal != 0 {
		return cardinal + 1
	}
	diagonal := 0
	if tileNameAt(names, width, height, x-1, y-1) == wanted {
		diagonal |= 1
	}
	if tileNameAt(names, width, height, x+1, y-1) == wanted {
		diagonal |= 2
	}
	if tileNameAt(names, width, height, x+1, y+1) == wanted {
		diagonal |= 4
	}
	if tileNameAt(names, width, height, x-1, y+1) == wanted {
		diagonal |= 8
	}
	if diagonal != 0 {
		return 17 + diagonal
	}
	return 0
}

func tileNameAt(names []string, width, height, x, y int) string {
	if x < 0 || y < 0 || x >= width || y >= height {
		return ""
	}
	return names[y*width+x]
}

func renderTerrainTile(canvas *image.NRGBA, tileHeight, tileX, tileY, scale int, layer terrainLayer, mask *image.NRGBA, maskBounds image.Rectangle) {
	noise := layer.texture
	imageY := tileHeight - tileY - 1
	for offsetY := 0; offsetY < scale; offsetY++ {
		for offsetX := 0; offsetX < scale; offsetX++ {
			x := tileX*scale + offsetX
			y := imageY*scale + offsetY
			maskX := maskBounds.Min.X + offsetX*maskBounds.Dx()/scale
			maskY := maskBounds.Min.Y + offsetY*maskBounds.Dy()/scale
			maskColor := mask.NRGBAAt(maskX, maskY)
			if maskColor.A == 0 {
				continue
			}
			worldX := (float64(tileX) + float64(offsetX)/float64(scale)) * worldUnitsPerTile
			worldZ := (float64(tileY) + 1 - float64(offsetY)/float64(scale)) * worldUnitsPerTile
			noiseX := positiveModulo(int(math.Floor(worldX/noiseWorldRepeat*float64(noise.Bounds().Dx()))), noise.Bounds().Dx())
			noiseY := positiveModulo(int(math.Floor(worldZ/noiseWorldRepeat*float64(noise.Bounds().Dy()))), noise.Bounds().Dy())
			noiseColor := noise.NRGBAAt(noise.Bounds().Min.X+noiseX, noise.Bounds().Min.Y+noiseY)
			source := minimapTerrainColor(layer.definition, maskColor, noiseColor)
			blendNRGBA(canvas, x, y, source)
		}
	}
}

func minimapTerrainColor(definition TileDefinition, maskColor, noiseColor color.NRGBA) color.NRGBA {
	alpha := uint8(uint16(maskColor.A) * uint16(noiseColor.A) / 255)
	if !definition.HasMinimapColor {
		return color.NRGBA{
			R: uint8(uint16(maskColor.R) * uint16(noiseColor.R) / 255),
			G: uint8(uint16(maskColor.G) * uint16(noiseColor.G) / 255),
			B: uint8(uint16(maskColor.B) * uint16(noiseColor.B) / 255),
			A: alpha,
		}
	}
	// Ocean tiles use the official per-depth minimap tint. Noise and map paper
	// contribute luminance while the tint retains the game's depth bands.
	noiseLight := (float64(noiseColor.R)*0.299 + float64(noiseColor.G)*0.587 + float64(noiseColor.B)*0.114) / 255
	maskLight := (float64(maskColor.R)*0.299 + float64(maskColor.G)*0.587 + float64(maskColor.B)*0.114) / 255
	factor := (0.72 + noiseLight*0.5) * (0.78 + maskLight*0.28)
	tinted := func(value uint8) uint8 {
		return uint8(math.Min(255, math.Round(float64(value)*factor)))
	}
	return color.NRGBA{
		R: tinted(definition.MinimapColor[0]),
		G: tinted(definition.MinimapColor[1]),
		B: tinted(definition.MinimapColor[2]),
		A: alpha,
	}
}

func renderRoads(canvas *image.NRGBA, parsed ParsedSave, scale int, assets *AssetSource) error {
	if len(parsed.Roads) == 0 {
		return nil
	}
	roadData, err := assets.ReadImage("roadnoise.tex")
	if err != nil {
		return err
	}
	pathData, err := assets.ReadImage("mini_pathnoise.tex")
	if err != nil {
		return err
	}
	roadTexture, err := DecodeKTEX(roadData)
	if err != nil {
		return fmt.Errorf("decode official road texture: %w", err)
	}
	pathTexture, err := DecodeKTEX(pathData)
	if err != nil {
		return fmt.Errorf("decode official path texture: %w", err)
	}
	for _, road := range parsed.Roads {
		texture := pathTexture
		width := math.Max(2, float64(scale)*0.52)
		if road.Kind == 3 {
			texture = roadTexture
			width = math.Max(3, float64(scale)*0.82)
		}
		points := smoothRoad(road.Points, parsed.TileWidth, parsed.TileHeight, scale)
		for index := 1; index < len(points); index++ {
			renderRoadSegment(canvas, points[index-1], points[index], width, texture)
		}
	}
	return nil
}

func smoothRoad(points []WorldPoint, mapWidth, mapHeight, scale int) []pixelPoint {
	if len(points) < 2 {
		return nil
	}
	converted := make([]pixelPoint, len(points))
	for index, point := range points {
		x, y := worldToPixel(point.X, point.Z, mapWidth, mapHeight, scale)
		converted[index] = pixelPoint{X: x, Y: y}
	}
	result := make([]pixelPoint, 0, len(points)*8)
	for index := 0; index < len(converted)-1; index++ {
		p0 := converted[max(0, index-1)]
		p1 := converted[index]
		p2 := converted[index+1]
		p3 := converted[min(len(converted)-1, index+2)]
		steps := max(4, int(math.Ceil(math.Hypot(p2.X-p1.X, p2.Y-p1.Y)/2)))
		for step := 0; step < steps; step++ {
			t := float64(step) / float64(steps)
			result = append(result, catmullRom(p0, p1, p2, p3, t))
		}
	}
	return append(result, converted[len(converted)-1])
}

type pixelPoint struct{ X, Y float64 }

func catmullRom(p0, p1, p2, p3 pixelPoint, t float64) pixelPoint {
	t2, t3 := t*t, t*t*t
	return pixelPoint{
		X: 0.5 * ((2 * p1.X) + (-p0.X+p2.X)*t + (2*p0.X-5*p1.X+4*p2.X-p3.X)*t2 + (-p0.X+3*p1.X-3*p2.X+p3.X)*t3),
		Y: 0.5 * ((2 * p1.Y) + (-p0.Y+p2.Y)*t + (2*p0.Y-5*p1.Y+4*p2.Y-p3.Y)*t2 + (-p0.Y+3*p1.Y-3*p2.Y+p3.Y)*t3),
	}
}

func renderRoadSegment(canvas *image.NRGBA, start, end pixelPoint, width float64, texture *image.NRGBA) {
	radius := width / 2
	minX := max(0, int(math.Floor(math.Min(start.X, end.X)-radius-1)))
	maxX := min(canvas.Bounds().Dx()-1, int(math.Ceil(math.Max(start.X, end.X)+radius+1)))
	minY := max(0, int(math.Floor(math.Min(start.Y, end.Y)-radius-1)))
	maxY := min(canvas.Bounds().Dy()-1, int(math.Ceil(math.Max(start.Y, end.Y)+radius+1)))
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			distance := pointSegmentDistance(float64(x)+0.5, float64(y)+0.5, start, end)
			alpha := math.Max(0, math.Min(1, radius+0.8-distance))
			if alpha <= 0 {
				continue
			}
			sample := texture.NRGBAAt(positiveModulo(x, texture.Bounds().Dx()), positiveModulo(y, texture.Bounds().Dy()))
			sample.A = uint8(float64(sample.A) * alpha * 0.92)
			blendNRGBA(canvas, x, y, sample)
		}
	}
}

func pointSegmentDistance(x, y float64, start, end pixelPoint) float64 {
	dx, dy := end.X-start.X, end.Y-start.Y
	lengthSquared := dx*dx + dy*dy
	if lengthSquared == 0 {
		return math.Hypot(x-start.X, y-start.Y)
	}
	t := math.Max(0, math.Min(1, ((x-start.X)*dx+(y-start.Y)*dy)/lengthSquared))
	return math.Hypot(x-(start.X+t*dx), y-(start.Y+t*dy))
}

func blendNRGBA(canvas *image.NRGBA, x, y int, source color.NRGBA) {
	if source.A == 0 || !image.Pt(x, y).In(canvas.Bounds()) {
		return
	}
	destination := canvas.NRGBAAt(x, y)
	sourceAlpha := uint32(source.A)
	inverse := uint32(255 - source.A)
	outputAlpha := sourceAlpha + uint32(destination.A)*inverse/255
	if outputAlpha == 0 {
		canvas.SetNRGBA(x, y, color.NRGBA{})
		return
	}
	blend := func(sourceChannel, destinationChannel uint8) uint8 {
		premultiplied := uint32(sourceChannel)*sourceAlpha + uint32(destinationChannel)*uint32(destination.A)*inverse/255
		return uint8(premultiplied / outputAlpha)
	}
	canvas.SetNRGBA(x, y, color.NRGBA{R: blend(source.R, destination.R), G: blend(source.G, destination.G), B: blend(source.B, destination.B), A: uint8(outputAlpha)})
}

func fillTile(canvas *image.NRGBA, tileX, tileY, tileHeight, scale int, shade color.NRGBA) {
	imageY := tileHeight - tileY - 1
	for offsetY := 0; offsetY < scale; offsetY++ {
		for offsetX := 0; offsetX < scale; offsetX++ {
			canvas.SetNRGBA(tileX*scale+offsetX, imageY*scale+offsetY, shade)
		}
	}
}

func unknownTerrainColor(value uint16) color.NRGBA {
	seed := uint32(value)*2654435761 + 0x9e3779b9
	return color.NRGBA{R: uint8(72 + seed%72), G: uint8(65 + (seed>>8)%64), B: uint8(70 + (seed>>16)%70), A: 255}
}

func positiveModulo(value, modulus int) int {
	if modulus <= 0 {
		return 0
	}
	result := value % modulus
	if result < 0 {
		result += modulus
	}
	return result
}

func worldToPixel(x, z float64, width, height, scale int) (float64, float64) {
	minX := -float64(width*worldUnitsPerTile) / 2
	maxZ := float64(height*worldUnitsPerTile) / 2
	pixelX := (x - minX) / worldUnitsPerTile * float64(scale)
	pixelY := (maxZ - z) / worldUnitsPerTile * float64(scale)
	return pixelX, pixelY
}
