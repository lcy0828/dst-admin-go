// Package landmarks defines code-owned destinations; callers cannot supply
// prefab selectors, topology patterns, functions or scripts.
package landmarks

import "math"

type Destination struct {
	ID      string
	Name    string
	Aliases []string
}

func Destinations() []Destination {
	return []Destination{
		{"spawn", "出生点", []string{"出生门", "绚丽之门"}},
		{"pig_king", "猪王", []string{"猪王村"}},
		{"lunar_island", "月岛", []string{"月亮岛"}},
		{"oasis", "绿洲", nil},
		{"dragonfly", "龙蝇沙漠", []string{"龙蝇"}},
		{"bee_queen", "蜂后", nil},
	}
}

func ID(name string) string {
	for _, item := range Destinations() {
		if name == item.ID || name == item.Name {
			return item.ID
		}
		for _, alias := range item.Aliases {
			if name == alias {
				return item.ID
			}
		}
	}
	return ""
}
func Name(id string) string {
	for _, item := range Destinations() {
		if id == item.ID {
			return item.Name
		}
	}
	return ""
}

type Point struct {
	ID         string  `json:"id"`
	LandmarkID string  `json:"landmarkId"`
	X          float64 `json:"x"`
	Z          float64 `json:"z"`
}
type Data struct {
	Items []Point `json:"items"`
	Count int     `json:"count"`
}

func (p Point) Valid() bool {
	return Name(p.LandmarkID) != "" && len(p.ID) > 0 && len(p.ID) <= 100 &&
		!math.IsNaN(p.X) && !math.IsNaN(p.Z) && !math.IsInf(p.X, 0) && !math.IsInf(p.Z, 0) && math.Abs(p.X) <= 100000 && math.Abs(p.Z) <= 100000
}
