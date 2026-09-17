package runtimefiles

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadConfigurationUsesFixedSharedFilesAndExplicitMissingEntries(t *testing.T) {
	root := t.TempDir()
	roomRoot := filepath.Join(root, "Cluster_1")
	worldRoot := filepath.Join(roomRoot, "Master")
	if err := os.MkdirAll(worldRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"cluster.ini":       "[NETWORK]\ncluster_name=Test\n",
		"adminlist.txt":     "KU_ADMIN\n",
		"cluster_token.txt": "must-not-leave-runtime\n",
	} {
		if err := os.WriteFile(filepath.Join(roomRoot, name), []byte(data), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	result, err := ReadConfiguration(context.Background(), root, "Cluster_1", "Master", ConfigurationScopeShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfiguration(ConfigurationScopeShared, result); err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 4 {
		t.Fatalf("files=%#v", result.Files)
	}
	values := make(map[string]bool, len(result.Files))
	for _, file := range result.Files {
		values[file.Name] = file.Exists
		if file.Name == "cluster_token.txt" {
			t.Fatal("cluster token escaped the fixed read surface")
		}
	}
	if !values["cluster.ini"] || !values["adminlist.txt"] || values["blocklist.txt"] || values["whitelist.txt"] {
		t.Fatalf("file presence=%#v", values)
	}
}

func TestValidateConfigurationRejectsTamperedData(t *testing.T) {
	root := t.TempDir()
	worldRoot := filepath.Join(root, "Cluster_1", "Master")
	if err := os.MkdirAll(worldRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worldRoot, "server.ini"), []byte("[SHARD]\nis_master=true\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worldRoot, "leveldataoverride.lua"), []byte("return { overrides = {} }\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	result, err := ReadConfiguration(context.Background(), root, "Cluster_1", "Master", ConfigurationScopeWorld)
	if err != nil {
		t.Fatal(err)
	}
	result.Files[0].Data[0] ^= 0xff
	if err := ValidateConfiguration(ConfigurationScopeWorld, result); err == nil {
		t.Fatal("tampered Runtime configuration was accepted")
	}
}

func TestReadModConfigurationUsesDedicatedScope(t *testing.T) {
	root := t.TempDir()
	worldRoot := filepath.Join(root, "Cluster_1", "Master")
	if err := os.MkdirAll(worldRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	expected := []byte("return { [\"workshop-100\"] = { enabled = true } }\n")
	if err := os.WriteFile(filepath.Join(worldRoot, "modoverrides.lua"), expected, 0o640); err != nil {
		t.Fatal(err)
	}
	result, err := ReadConfiguration(context.Background(), root, "Cluster_1", "Master", ConfigurationScopeMod)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfiguration(ConfigurationScopeMod, result); err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files[0].Name != "modoverrides.lua" || string(result.Files[0].Data) != string(expected) {
		t.Fatalf("result=%#v", result)
	}
}

func TestReadConfigurationReturnsTokenStatusWithoutSecret(t *testing.T) {
	root := t.TempDir()
	worldRoot := filepath.Join(root, "Cluster_1", "Master")
	if err := os.MkdirAll(worldRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Cluster_1", "cluster_token.txt"), []byte("must-not-leave-runtime\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ReadConfiguration(context.Background(), root, "Cluster_1", "Master", ConfigurationScopeTokenStatus)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfiguration(ConfigurationScopeTokenStatus, result); err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 0 || result.TokenStatus == nil || !result.TokenStatus.Configured || result.TokenStatus.MaskedValue != "****time" {
		t.Fatalf("status = %#v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "must-not-leave-runtime") {
		t.Fatal("token escaped the runtime status response")
	}
}
