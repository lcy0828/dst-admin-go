package runtimefiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEffectiveShardIdentityPreservesImportedConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, cluster, server, wantID, wantErr string
	}{
		{"imported master", "[SHARD]\nshard_enabled = true\n", "[SHARD]\nis_master = true\nid = 10001\n", "1", ""},
		{"imported secondary", "[SHARD]\nshard_enabled = true\n", "[SHARD]\nis_master = false\nid = 3152324993\n", "3152324993", ""},
		{"unsharded master", "[SHARD]\nshard_enabled = false\n", "[SHARD]\nis_master = true\nid = 10001\n", "0", ""},
		{"master without configured ID", "[SHARD]\nshard_enabled = true\n", "[SHARD]\nis_master = true\n", "1", ""},
		{"legacy cluster", "[NETWORK]\ncluster_name = Legacy\n", "[SHARD]\nis_master = true\nid = 10001\n", "10001", ""},
		{"legacy shard role", "[SHARD]\nshard_enabled = true\n", "[SHARD]\nid = 10001\n", "10001", ""},
		{"invalid master flag", "[SHARD]\nshard_enabled = true\n", "[SHARD]\nis_master = invalid\nid = 10001\n", "", "parse server.ini is_master"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			room := t.TempDir()
			world := filepath.Join(room, "Master")
			if err := os.Mkdir(world, 0750); err != nil {
				t.Fatal(err)
			}
			files := map[string]string{filepath.Join(room, "cluster.ini"): tc.cluster, filepath.Join(world, "server.ini"): tc.server}
			for path, data := range files {
				if err := os.WriteFile(path, []byte(data), 0640); err != nil {
					t.Fatal(err)
				}
			}
			id, err := ReadConfiguredShardID(world)
			if id != tc.wantID || (tc.wantErr == "" && err != nil) || (tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr))) {
				t.Fatalf("identity=%q err=%v, want %q / %q", id, err, tc.wantID, tc.wantErr)
			}
			for path, data := range files {
				written, err := os.ReadFile(path)
				if err != nil || string(written) != data {
					t.Fatalf("identity read changed configuration %s: %v", path, err)
				}
			}
		})
	}
}
