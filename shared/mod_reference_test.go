package shared

import "testing"

func TestModReferenceRoundTrip(t *testing.T) {
	for input, want := range map[string]ModReference{
		"123":              {"123", "workshop", "workshop-123"},
		"fishdried_custom": {"local:fishdried_custom", "local", "fishdried_custom"},
		"local:123":        {"local:123", "local", "123"},
		"local:小鱼干":        {"local:小鱼干", "local", "小鱼干"},
		"workshop-custom":  {"local:workshop-custom", "local", "workshop-custom"},
	} {
		got, ok := ParseModReference(input)
		if !ok || got != want {
			t.Fatalf("%q: %#v %v", input, got, ok)
		}
		back, ok := ModReferenceFromKey(got.DirectoryName)
		if !ok || back != want {
			t.Fatalf("key round trip: %#v", back)
		}
	}
	for _, input := range []string{"", ".", "..", "local:", "local:workshop-123", "workshop-123", "local:../escape", "local:a/b", "local:a\\b", "local:a\x00b", "123456789012345678901"} {
		if ref, ok := ParseModReference(input); ok {
			t.Fatalf("accepted %q: %#v", input, ref)
		}
	}
}
