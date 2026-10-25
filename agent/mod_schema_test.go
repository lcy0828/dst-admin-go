package agent

import (
	"os"
	"path/filepath"
	"testing"

	"dont/shared"
)

func TestModSchemaReadsInstallationFilesAndSiblingModulesWithoutPublication(t *testing.T) {
	agent, installation := newModOperationAgent(t)
	root := filepath.Join(installation.WorkshopContentPath, "2189004162")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"modinfo.lua": "configuration_options = require('options')",
		"options.lua": "return {{name='language',default='zh',options={{data='zh',description='Chinese'}}}}",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sequence := 0
	result, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModSchemaRead, shared.RuntimeModRequest{WorkshopID: "2189004162"})
	if err != nil || result.Schema == nil || !result.Complete || result.Schema.WorkshopID != "2189004162" || result.Schema.Parser != "go" {
		t.Fatalf("schema=%#v, err=%v", result, err)
	}
	options, ok := result.Schema.Options.([]interface{})
	if !ok || len(options) != 1 || options[0].(map[string]interface{})["name"] != "language" {
		t.Fatalf("options=%#v", result.Schema.Options)
	}
	if len(agent.modDistributions) != 0 {
		t.Fatal("schema read initialized Mod publication")
	}
	if _, err := os.Stat(installation.ModStatePath); !os.IsNotExist(err) {
		t.Fatalf("schema read created publication state: %v", err)
	}
	for _, request := range []shared.RuntimeModRequest{
		{WorkshopID: "999"}, {WorkshopID: "../2189004162"}, {WorkshopID: "2189004162", Validate: true},
	} {
		if _, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModSchemaRead, request); err == nil {
			t.Fatalf("accepted missing file or invalid request: %#v", request)
		}
	}
}

func TestLocalModProtocolIsExplicitAndDoesNotChangeLegacyInventory(t *testing.T) {
	agent, installation := newModOperationAgent(t)
	root := filepath.Join(installation.ServerPath, "mods", "fishdried_custom")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "modinfo.lua"), []byte(`name=folder_name; version="1.2"; configuration_options={{name="mode",default="easy"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	sequence := 0
	result, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModSchemaRead, shared.RuntimeModRequest{ModID: "local:fishdried_custom"})
	if err != nil || result.Schema == nil || result.Schema.ModID != "local:fishdried_custom" || result.Schema.Name != "fishdried_custom" || result.Schema.WorkshopID != "" {
		t.Fatalf("%+v %v", result, err)
	}
	legacy, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModFilesInventory, shared.RuntimeModRequest{})
	if err != nil || len(legacy.Files.Mods) != 0 {
		t.Fatalf("legacy includes local mods: %+v %v", legacy, err)
	}
	current, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModFilesInventory, shared.RuntimeModRequest{IncludeLocal: true})
	if err != nil || current.Files.Mods["local:fishdried_custom"].Status != shared.RuntimeModFileReady {
		t.Fatalf("%+v %v", current, err)
	}
	for _, action := range []shared.RuntimeAction{shared.RuntimeActionModDownload, shared.RuntimeActionModFetch, shared.RuntimeActionModLink} {
		if err := validateModOperationPayload(shared.RuntimeOperationRequest{Action: action, Mod: &shared.RuntimeModRequest{ModID: "local:fishdried_custom", WorkshopID: "123"}}); err == nil {
			t.Fatalf("local fields accepted for Steam mutation %s", action)
		}
	}
	if len(agent.modDistributions) != 0 {
		t.Fatal("read initialized content publication")
	}
}

func TestModOverridesReadDistinguishesMissingFromExistingEmpty(t *testing.T) {
	agent, installation := newModOperationAgent(t)
	sequence := 0
	request := shared.RuntimeModRequest{RoomDirectory: "Cluster_1", WorldDirectory: "Master"}
	missing, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModOverridesRead, request)
	if err != nil || missing.Overrides == nil || !missing.Overrides.Missing || !missing.Overrides.Complete || len(missing.Overrides.Data) != 0 {
		t.Fatalf("missing config response=%+v err=%v", missing, err)
	}
	path := filepath.Join(installation.SavePath, "Cluster_1", "Master", "modoverrides.lua")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModOverridesRead, request); err == nil {
		t.Fatal("existing empty config reported as missing")
	}
	if err := os.WriteFile(path, []byte("return {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	current, err := executeModRequest(t, agent, &sequence, shared.RuntimeActionModOverridesRead, request)
	if err != nil || current.Overrides == nil || current.Overrides.Missing || string(current.Overrides.Data) != "return {}\n" {
		t.Fatalf("existing config=%+v err=%v", current, err)
	}
}
