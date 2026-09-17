package moddistribution

import (
	"errors"
	"strings"
	"testing"
	"time"

	"dont/internal/steamvdf"
)

func TestComposeWorkshopManifestRegistersControllerArtifact(t *testing.T) {
	current := []byte(`"AppWorkshop"
{
  "appid" "322330"
  "WorkshopItemsInstalled"
  {
    "111" { "size" "10" "timeupdated" "100" "manifest" "9001" }
  }
  "WorkshopItemDetails" {}
}`)
	installation := TrustedInstallation{
		WorkshopContentPath:  "/opt/dst/workshop/steamapps/workshop/content/322330",
		WorkshopManifestPath: "/opt/dst/workshop/steamapps/workshop/appworkshop_322330.acf",
	}
	encoded, err := composeWorkshopManifest(current, installation, []ModVersion{{
		WorkshopID: "3687959533", TreeSHA256: strings.Repeat("a", 64),
		Metadata: Metadata{PublishedFileSize: 45090, SteamManifestID: "5198458395439210153", SteamUpdatedAt: time.Unix(1779124745, 0)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := steamvdf.Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	app := root["AppWorkshop"].(map[string]interface{})
	installed := app["WorkshopItemsInstalled"].(map[string]interface{})
	item := installed["3687959533"].(map[string]interface{})
	if item["manifest"] != "5198458395439210153" || item["size"] != "45090" || app["SizeOnDisk"] != "45100" {
		t.Fatalf("unexpected registration: app=%#v item=%#v", app, item)
	}
}

func TestValidateWorkshopManifestInventoryRejectsOrphanContent(t *testing.T) {
	current := []byte(`"AppWorkshop"
{
  "appid" "322330"
  "WorkshopItemsInstalled"
  {
    "111" { "size" "10" "timeupdated" "100" "manifest" "9001" }
  }
}`)
	err := validateWorkshopManifestInventory(current, map[string]string{"111": "tree-a", "3687959533": "tree-b"})
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "3687959533") {
		t.Fatalf("missing Workshop registration was accepted: %v", err)
	}
}

func TestComposeWorkshopManifestRejectsUnregisteredTransferredContent(t *testing.T) {
	installation := TrustedInstallation{
		WorkshopContentPath:  "/opt/dst/workshop/steamapps/workshop/content/322330",
		WorkshopManifestPath: "/opt/dst/workshop/steamapps/workshop/appworkshop_322330.acf",
	}
	_, err := composeWorkshopManifest(nil, installation, []ModVersion{{WorkshopID: "3687959533", TreeSHA256: strings.Repeat("a", 64)}})
	if err == nil || !strings.Contains(err.Error(), "3687959533") {
		t.Fatalf("expected Workshop registration error, got %v", err)
	}
}
