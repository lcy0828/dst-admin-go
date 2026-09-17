package luajit

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestUpstreamCatalogRequiresOfficialLinuxAssetAndChecksum(t *testing.T) {
	valid := map[string]any{"tag_name": "v3.1.0", "assets": []map[string]any{{"name": "linux_Mod.zip", "size": 1024, "digest": "sha256:" + strings.Repeat("a", 64), "browser_download_url": "https://github.com/fesily/DontStarveLuaJIT2/releases/download/v3.1.0/linux_Mod.zip"}}}
	data, _ := json.Marshal(valid)
	r, err := upstreamRelease(data)
	if err != nil || r.Version != "3.1.0" || r.Channel != "upstream" {
		t.Fatalf("release=%+v err=%v", r, err)
	}
	for _, bad := range []string{
		strings.Replace(string(data), "https://github.com/fesily/", "https://example.org/fesily/", 1),
		strings.Replace(string(data), "sha256:"+strings.Repeat("a", 64), "", 1),
		strings.Replace(string(data), "linux_Mod.zip", "windows_Mod.zip", 1),
	} {
		if _, err := upstreamRelease([]byte(bad)); err == nil {
			t.Fatal("invalid upstream metadata accepted")
		}
	}
}
func TestPackageWithoutCustomContractIsAccepted(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	data := fixtureArchive(t, func(files map[string][]byte) { delete(files, "dst_admin_runtime_modes.v1") })
	if _, err := store.Save(context.Background(), bytes.NewReader(data), ""); err != nil {
		t.Fatal(err)
	}
}
func TestAvailablePrefersUpstreamWithoutMaterializingAnyPackage(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	releases, err := store.Available()
	if err != nil || len(releases) < 2 || releases[0].Channel != "upstream" || releases[0].SourceURL == "" {
		t.Fatalf("catalog: %+v %v", releases, err)
	}
	cached, err := store.List()
	if err != nil || len(cached) != 0 {
		t.Fatal("catalog materialized a package", err)
	}
}
