package maprenderer

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
)

const maxImageDimension = 8192

var terrainColors = map[uint16]color.RGBA{
	0: {R: 28, G: 31, B: 32, A: 255}, 1: {R: 31, G: 34, B: 34, A: 255},
	2: {R: 112, G: 107, B: 94, A: 255}, 3: {R: 105, G: 112, B: 108, A: 255},
	4: {R: 116, G: 101, B: 77, A: 255}, 5: {R: 157, G: 139, B: 84, A: 255},
	6: {R: 83, G: 116, B: 59, A: 255}, 7: {R: 48, G: 76, B: 43, A: 255},
	8: {R: 68, G: 59, B: 57, A: 255}, 9: {R: 25, G: 28, B: 29, A: 255},
	10: {R: 121, G: 91, B: 57, A: 255}, 11: {R: 89, G: 79, B: 105, A: 255},
	12: {R: 145, G: 145, B: 139, A: 255}, 13: {R: 97, G: 88, B: 75, A: 255},
	14: {R: 78, G: 68, B: 91, A: 255}, 15: {R: 76, G: 84, B: 49, A: 255},
	16: {R: 83, G: 84, B: 80, A: 255}, 17: {R: 81, G: 66, B: 48, A: 255},
	18: {R: 119, G: 116, B: 106, A: 255}, 19: {R: 92, G: 91, B: 84, A: 255},
	20: {R: 83, G: 69, B: 92, A: 255}, 21: {R: 75, G: 61, B: 84, A: 255},
	22: {R: 60, G: 59, B: 59, A: 255}, 23: {R: 53, G: 55, B: 53, A: 255},
	24: {R: 91, G: 56, B: 54, A: 255}, 25: {R: 69, G: 89, B: 68, A: 255},
	30: {R: 121, G: 86, B: 40, A: 255}, 31: {R: 147, G: 103, B: 65, A: 255},
	32: {R: 54, G: 47, B: 46, A: 255}, 39: {R: 92, G: 72, B: 48, A: 255},
	42: {R: 95, G: 119, B: 117, A: 255}, 43: {R: 88, G: 139, B: 145, A: 255},
	44: {R: 165, G: 158, B: 192, A: 255}, 45: {R: 144, G: 105, B: 80, A: 255},
	46: {R: 73, G: 101, B: 95, A: 255}, 47: {R: 77, G: 69, B: 54, A: 255},
	200: {R: 15, G: 26, B: 31, A: 255}, 201: {R: 42, G: 95, B: 105, A: 255},
	202: {R: 36, G: 86, B: 99, A: 255}, 203: {R: 25, G: 67, B: 82, A: 255},
	204: {R: 18, G: 49, B: 65, A: 255}, 205: {R: 43, G: 89, B: 112, A: 255},
	206: {R: 50, G: 96, B: 116, A: 255}, 207: {R: 30, G: 59, B: 62, A: 255},
	208: {R: 158, G: 166, B: 169, A: 255}, 247: {R: 29, G: 32, B: 33, A: 255},
}

func pixelsPerTile(width, height int) int {
	scale := 4
	for scale > 1 && (width*scale > maxImageDimension || height*scale > maxImageDimension) {
		scale--
	}
	return scale
}

func RenderTerrain(output io.Writer, parsed ParsedSave) (int, error) {
	scale := pixelsPerTile(parsed.TileWidth, parsed.TileHeight)
	canvas := image.NewRGBA(image.Rect(0, 0, parsed.TileWidth*scale, parsed.TileHeight*scale))
	unknown := 0
	for tileY := 0; tileY < parsed.TileHeight; tileY++ {
		for tileX := 0; tileX < parsed.TileWidth; tileX++ {
			index := tileY*parsed.TileWidth + tileX
			value := parsed.TileIDs[index]
			shade, ok := terrainColors[value]
			if !ok {
				unknown++
				shade = unknownTerrainColor(value)
			}
			imageX := tileX
			imageY := parsed.TileHeight - tileY - 1
			for offsetY := 0; offsetY < scale; offsetY++ {
				for offsetX := 0; offsetX < scale; offsetX++ {
					canvas.SetRGBA(imageX*scale+offsetX, imageY*scale+offsetY, shade)
				}
			}
		}
	}
	if err := png.Encode(output, canvas); err != nil {
		return 0, fmt.Errorf("encode terrain PNG: %w", err)
	}
	return unknown, nil
}

func unknownTerrainColor(value uint16) color.RGBA {
	seed := uint32(value)*2654435761 + 0x9e3779b9
	return color.RGBA{R: uint8(72 + seed%72), G: uint8(65 + (seed>>8)%64), B: uint8(70 + (seed>>16)%70), A: 255}
}

func worldToPixel(x, z float64, width, height, scale int) (float64, float64) {
	minX := -float64(width*worldUnitsPerTile) / 2
	maxZ := float64(height*worldUnitsPerTile) / 2
	pixelX := (x - minX) / worldUnitsPerTile * float64(scale)
	pixelY := (maxZ - z) / worldUnitsPerTile * float64(scale)
	return pixelX, pixelY
}
