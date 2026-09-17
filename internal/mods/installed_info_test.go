package mods

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInstalledModInfoPrefersManualRuntimeFiles(t *testing.T) {
	root := t.TempDir()
	workshop, server := filepath.Join(root, "workshop"), filepath.Join(root, "server")
	for path, content := range map[string]string{
		filepath.Join(workshop, "100", "modinfo.lua"):                "configuration_options={{name='cached',default=false}}",
		filepath.Join(server, "mods", "workshop-100", "modinfo.lua"): "configuration_options={{name='manual',default=true}}",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	parsed, err := ParseInstalledModInfo(context.Background(), NewDualParser("", ""), workshop, server, "100")
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := ConfigurationFromParsed("room", "world", "100", []byte(`return {["workshop-100"]={enabled=false,configuration_options={manual=false,custom="keep"}}}`), parsed)
	if err != nil || len(configuration.Fields) != 1 || configuration.Fields[0].Key != "manual" || configuration.Values["manual"] != false || configuration.UnknownValues["custom"] != "keep" || configuration.Enabled {
		t.Fatalf("configuration=%#v, err=%v", configuration, err)
	}
	if _, err := ParseInstalledModInfo(context.Background(), NewDualParser("", ""), workshop, server, "999"); !errors.Is(err, ErrModInfoUnavailable) {
		t.Fatalf("missing file error=%v", err)
	}
	if err := os.Symlink(filepath.Join(server, "mods", "workshop-100"), filepath.Join(workshop, "200")); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseInstalledModInfo(context.Background(), NewDualParser("", ""), workshop, server, "200"); err == nil {
		t.Fatal("accepted a Workshop directory escaping its trusted root")
	}
}
