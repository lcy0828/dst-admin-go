package console

import (
	"testing"
)

func TestActionCatalogUsesOnlyCodeOwnedEnumSuggestions(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range BuiltinWorkbenchDefinitions() {
		for _, a := range d.Actions {
			t.Run(a.ID, func(t *testing.T) {
				if seen[a.ID] || a.Risk != d.Risk || a.Title == "" || len(a.Parameters) != 1 {
					t.Fatal(a)
				}
				seen[a.ID] = true
				for field, value := range a.Parameters {
					valid := false
					for _, p := range d.Parameters {
						if p.Name == field && p.Type == "enum" {
							for _, option := range p.Options {
								if option == value {
									valid = true
								}
							}
						}
					}
					if !valid {
						t.Fatalf("invalid fixed parameter: %s=%s", field, value)
					}
				}
			})
		}
	}
	if len(seen) < 90 {
		t.Fatalf("incomplete action descriptions: %d", len(seen))
	}
}
