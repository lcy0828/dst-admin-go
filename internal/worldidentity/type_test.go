package worldidentity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseTypeKeepsWorldTypeIndependentFromShardRole(t *testing.T) {
	tests := []struct {
		name string
		data string
		want Type
	}{
		{name: "forest", data: `return { location = "forest", worldgen_id = "SURVIVAL_TOGETHER" }`, want: TypeForest},
		{name: "cave", data: `return { location = "cave", settings_id = "DST_CAVE" }`, want: TypeCave},
		{name: "unknown", data: `return { override_enabled = true, overrides = {} }`, want: TypeUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ParseType([]byte(test.data)); got != test.want {
				t.Fatalf("ParseType()=%q want=%q", got, test.want)
			}
		})
	}
}

func TestResolveTypeDoesNotAssumeGenericWorldIsForest(t *testing.T) {
	root := t.TempDir()
	if got := ResolveType(root, "SurfaceSeven"); got != TypeUnknown {
		t.Fatalf("generic directory type=%q want=%q", got, TypeUnknown)
	}
	if got := ResolveType(root, "Deep_Caves_2"); got != TypeCave {
		t.Fatalf("legacy Cave directory type=%q want=%q", got, TypeCave)
	}
	if err := os.WriteFile(filepath.Join(root, "leveldataoverride.lua"), []byte(`return { location = "forest" }`), 0o640); err != nil {
		t.Fatal(err)
	}
	if got := ResolveType(root, "Deep_Caves_2"); got != TypeForest {
		t.Fatalf("explicit override type=%q want=%q", got, TypeForest)
	}
}

func TestResolveReportedTypeRespectsExplicitUnknown(t *testing.T) {
	if got := ResolveReportedType("unknown", "Caves"); got != TypeUnknown {
		t.Fatalf("explicit unknown type=%q want=%q", got, TypeUnknown)
	}
	if got := ResolveReportedType("", "Caves"); got != TypeCave {
		t.Fatalf("old Agent Cave fallback=%q want=%q", got, TypeCave)
	}
}
