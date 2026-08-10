package maprenderer

import (
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"math"
)

type Atlas struct {
	Texture  string
	Elements map[string]AtlasElement
}

type AtlasElement struct {
	Name   string
	U1, U2 float64
	V1, V2 float64
}

type atlasDocument struct {
	Texture struct {
		Filename string `xml:"filename,attr"`
	} `xml:"Texture"`
	Elements struct {
		Items []struct {
			Name string  `xml:"name,attr"`
			U1   float64 `xml:"u1,attr"`
			U2   float64 `xml:"u2,attr"`
			V1   float64 `xml:"v1,attr"`
			V2   float64 `xml:"v2,attr"`
		} `xml:"Element"`
	} `xml:"Elements"`
}

func ParseAtlas(data []byte) (Atlas, error) {
	var document atlasDocument
	if err := xml.Unmarshal(data, &document); err != nil {
		return Atlas{}, fmt.Errorf("decode atlas XML: %w", err)
	}
	if document.Texture.Filename == "" || len(document.Elements.Items) == 0 {
		return Atlas{}, errors.New("atlas XML is missing its texture or elements")
	}
	result := Atlas{Texture: document.Texture.Filename, Elements: make(map[string]AtlasElement, len(document.Elements.Items))}
	for _, item := range document.Elements.Items {
		values := []float64{item.U1, item.U2, item.V1, item.V2}
		valid := item.Name != ""
		for _, value := range values {
			valid = valid && !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
		}
		if !valid || item.U1 == item.U2 || item.V1 == item.V2 {
			return Atlas{}, fmt.Errorf("atlas element %q has invalid coordinates", item.Name)
		}
		result.Elements[item.Name] = AtlasElement{Name: item.Name, U1: item.U1, U2: item.U2, V1: item.V1, V2: item.V2}
	}
	return result, nil
}

func (e AtlasElement) Bounds(texture image.Image) (image.Rectangle, error) {
	if texture == nil {
		return image.Rectangle{}, errors.New("atlas texture is missing")
	}
	bounds := texture.Bounds()
	left := bounds.Min.X + int(math.Floor(math.Min(e.U1, e.U2)*float64(bounds.Dx())))
	right := bounds.Min.X + int(math.Ceil(math.Max(e.U1, e.U2)*float64(bounds.Dx())))
	top := bounds.Min.Y + int(math.Floor(math.Min(e.V1, e.V2)*float64(bounds.Dy())))
	bottom := bounds.Min.Y + int(math.Ceil(math.Max(e.V1, e.V2)*float64(bounds.Dy())))
	result := image.Rect(left, top, right, bottom).Intersect(bounds)
	if result.Empty() {
		return image.Rectangle{}, fmt.Errorf("atlas element %q has empty bounds", e.Name)
	}
	return result, nil
}
