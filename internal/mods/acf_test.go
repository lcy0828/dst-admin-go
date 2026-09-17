package mods

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/internal/steamvdf"
)

func TestLoadWorkshopManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "appworkshop_322330.acf")
	data := `"AppWorkshop"
{
  "appid" "322330"
  "WorkshopItemsInstalled"
  {
    "378160973"
    {
      "manifest" "12345"
      "timeupdated" "1723082400"
    }
    "not-a-mod" { "manifest" "ignored" }
  }
}`
	if err := os.WriteFile(path, []byte(data), 0640); err != nil {
		t.Fatal(err)
	}
	items, err := loadWorkshopManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := items["378160973"]
	if !ok || item.ManifestID != "12345" {
		t.Fatalf("unexpected item: %#v", item)
	}
	if want := time.Unix(1723082400, 0).UTC(); !item.UpdatedAt.Equal(want) {
		t.Fatalf("updated at = %s, want %s", item.UpdatedAt, want)
	}
	if _, ok := items["not-a-mod"]; ok {
		t.Fatal("invalid Mod ID was accepted")
	}
}

func TestParseValveKeyValuesRejectsMalformedInput(t *testing.T) {
	for _, input := range []string{`"key" {`, `}`, `key value`, `"key" / bad`} {
		if _, err := steamvdf.Parse([]byte(input)); err == nil {
			t.Fatalf("expected malformed input to fail: %q", input)
		}
	}
}
