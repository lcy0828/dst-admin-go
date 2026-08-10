package maprenderer

import (
	"fmt"
	"image"
	"image/draw"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	iconSpriteWidth  = 2048
	iconSpriteLimit  = 4096
	iconSpriteGutter = 1
)

type atlasIcon struct {
	name  string
	image *image.NRGBA
}

var officialIconAliases = map[string]string{
	"multiplayer_portal": "portal_dst.png",
	"resurrectionstone":  "resurrection_stone.png",
	"monkeyqueen":        "monkey_queen.png",
	"moonspiderden":      "spidermoonden.png",
	"charlie_lecturn":    "charlie_lectern.png",
}

func RenderOfficialIcons(parsed *ParsedSave, assets *AssetSource) (*image.NRGBA, int, error) {
	needed := make(map[string]string)
	for _, feature := range parsed.Features {
		name := strings.ToLower(feature.Prefab) + ".png"
		if alias := officialIconAliases[strings.ToLower(feature.Prefab)]; alias != "" {
			name = alias
		}
		needed[name] = feature.Prefab
	}
	found := make(map[string]*image.NRGBA)
	for index := 0; index < 3; index++ {
		suffix := ""
		if index > 0 {
			suffix = fmt.Sprintf("%d", index)
		}
		xmlPath := filepath.ToSlash(filepath.Join("minimap", "minimap_data"+suffix+".xml"))
		if _, err := os.Stat(filepath.Join(assets.DataRoot, filepath.FromSlash(xmlPath))); os.IsNotExist(err) {
			continue
		}
		xmlData, err := assets.Read(xmlPath)
		if err != nil {
			return nil, 0, err
		}
		atlas, err := ParseAtlas(xmlData)
		if err != nil {
			return nil, 0, fmt.Errorf("parse official minimap atlas %s: %w", xmlPath, err)
		}
		texturePath := filepath.ToSlash(filepath.Join("minimap", filepath.Base(atlas.Texture)))
		textureData, err := assets.Read(texturePath)
		if err != nil {
			return nil, 0, err
		}
		texture, err := DecodeKTEX(textureData)
		if err != nil {
			return nil, 0, fmt.Errorf("decode official minimap atlas %s: %w", texturePath, err)
		}
		for name := range needed {
			if found[name] != nil {
				continue
			}
			element, ok := atlas.Elements[name]
			if !ok {
				continue
			}
			bounds, err := element.Bounds(texture)
			if err != nil {
				return nil, 0, err
			}
			icon := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
			draw.Draw(icon, icon.Bounds(), texture, bounds.Min, draw.Src)
			found[name] = icon
		}
	}
	icons := make([]atlasIcon, 0, len(found))
	for name, icon := range found {
		icons = append(icons, atlasIcon{name: name, image: icon})
	}
	sort.Slice(icons, func(left, right int) bool {
		if icons[left].image.Bounds().Dy() == icons[right].image.Bounds().Dy() {
			return icons[left].name < icons[right].name
		}
		return icons[left].image.Bounds().Dy() > icons[right].image.Bounds().Dy()
	})
	placements, height, err := packOfficialIcons(icons)
	if err != nil {
		return nil, 0, err
	}
	width := iconSpriteWidth
	if len(icons) == 0 {
		width, height = 1, 1
	}
	sprite := image.NewNRGBA(image.Rect(0, 0, width, height))
	for _, icon := range icons {
		placement := placements[icon.name]
		draw.Draw(sprite, image.Rect(placement.X, placement.Y, placement.X+placement.Width, placement.Y+placement.Height), icon.image, image.Point{}, draw.Src)
	}
	matched := 0
	for index := range parsed.Features {
		name := strings.ToLower(parsed.Features[index].Prefab) + ".png"
		if alias := officialIconAliases[strings.ToLower(parsed.Features[index].Prefab)]; alias != "" {
			name = alias
		}
		placement, ok := placements[name]
		if !ok {
			continue
		}
		value := placement
		parsed.Features[index].Icon = &value
		matched++
	}
	return sprite, matched, nil
}

func packOfficialIcons(icons []atlasIcon) (map[string]IconReference, int, error) {
	result := make(map[string]IconReference, len(icons))
	x, y, rowHeight := iconSpriteGutter, iconSpriteGutter, 0
	for _, icon := range icons {
		width, height := icon.image.Bounds().Dx(), icon.image.Bounds().Dy()
		if width <= 0 || height <= 0 || width+iconSpriteGutter*2 > iconSpriteWidth || height+iconSpriteGutter*2 > iconSpriteLimit {
			return nil, 0, fmt.Errorf("official minimap icon %s exceeds sprite limits", icon.name)
		}
		if x+width+iconSpriteGutter > iconSpriteWidth {
			x = iconSpriteGutter
			y += rowHeight + iconSpriteGutter
			rowHeight = 0
		}
		if y+height+iconSpriteGutter > iconSpriteLimit {
			return nil, 0, errorsNewIconSpriteLimit()
		}
		result[icon.name] = IconReference{X: x, Y: y, Width: width, Height: height}
		x += width + iconSpriteGutter
		rowHeight = max(rowHeight, height)
	}
	return result, max(1, y+rowHeight+iconSpriteGutter), nil
}

func errorsNewIconSpriteLimit() error {
	return fmt.Errorf("official minimap icons exceed the %dx%d sprite limit", iconSpriteWidth, iconSpriteLimit)
}
