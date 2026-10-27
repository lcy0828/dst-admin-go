package runtimefiles

import (
	"context"
	"dont/shared"
	"os"
	"path/filepath"
	"testing"
)

func TestActivityReadsAreIncrementalAndConfined(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "room", "Master", "save", "mod_config_data", "dst-admin")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"activity-manifest.json", "activity-01.json", "activity-02.json"} {
		if e := os.WriteFile(filepath.Join(dir, name), []byte(`{"schemaVersion":1}`), 0600); e != nil {
			t.Fatal(e)
		}
	}
	b, e := ReadArtifacts(context.Background(), root, "room", "Master", shared.ArtifactPlayerActivity)
	if e != nil || len(b.Artifacts) != 1 || b.Artifacts[0].Name != "activity-manifest.json" {
		t.Fatalf("default scanned spool %+v %v", b, e)
	}
	b, e = ReadArtifacts(context.Background(), root, "room", "Master", shared.ArtifactPlayerActivity, "activity-02.json")
	if e != nil || len(b.Artifacts) != 1 || b.Artifacts[0].Name != "activity-02.json" {
		t.Fatalf("selected read %+v %v", b, e)
	}
	b.Artifacts[0].Data[0] = '!'
	if ValidateArtifactBundle(shared.ArtifactPlayerActivity, b) == nil {
		t.Fatal("corruption accepted")
	}
	for _, names := range [][]string{{"../cluster_token.txt"}, {"/etc/passwd"}, {"activity-65.json"}, {"activity-01.json", "activity-01.json"}, make([]string, 9)} {
		if _, e := RequestedArtifactNames(shared.ArtifactPlayerActivity, names); e == nil {
			t.Fatalf("unsafe selection %v", names)
		}
	}
	target := filepath.Join(root, "outside")
	os.WriteFile(target, []byte(`{"secret":true}`), 0600)
	os.Symlink(target, filepath.Join(dir, "activity-03.json"))
	if _, e := ReadArtifacts(context.Background(), root, "room", "Master", shared.ArtifactPlayerActivity, "activity-03.json"); e == nil {
		t.Fatal("symlink escaped namespace")
	}
}
