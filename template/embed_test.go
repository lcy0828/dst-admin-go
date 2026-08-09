package template

import (
	"bytes"
	"testing"
)

func TestLevelDataOverrideContainsRequiredWorldGenerationDefaults(t *testing.T) {
	tests := []struct {
		worldType string
		required  [][]byte
	}{
		{worldType: "forest", required: [][]byte{[]byte(`location="forest"`), []byte(`task_set="default"`), []byte(`start_location="default"`), []byte(`required_prefabs={ "multiplayer_portal" }`)}},
		{worldType: "cave", required: [][]byte{[]byte(`location="cave"`), []byte(`task_set="cave_default"`), []byte(`start_location="caves"`), []byte(`required_prefabs={ "multiplayer_portal" }`)}},
	}
	for _, test := range tests {
		t.Run(test.worldType, func(t *testing.T) {
			data, err := LevelDataOverride(test.worldType)
			if err != nil {
				t.Fatal(err)
			}
			for _, required := range test.required {
				if !bytes.Contains(data, required) {
					t.Fatalf("template does not contain %q", required)
				}
			}
		})
	}
}

func TestLevelDataOverrideRejectsUnknownWorldType(t *testing.T) {
	if _, err := LevelDataOverride("ocean"); err == nil {
		t.Fatal("unknown world type was accepted")
	}
}
