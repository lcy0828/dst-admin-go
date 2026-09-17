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
