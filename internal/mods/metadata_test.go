package mods

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLibraryReadsLiteralMetadataWithoutExecutingModCode(t *testing.T) {
	service, _, _ := newConfigTestService(t)
	path := filepath.Join(service.downloadedPath("378160973"), "modinfo.lua")
	if err := os.WriteFile(path, []byte(`
name = "Local name"
version = "4.2"
author = "Author"
description = [[Description]]
while true do end
`), 0o640); err != nil {
		t.Fatal(err)
	}
	values := literalModMetadata(path)
	if values["version"] != "4.2" || values["description"] != "Description" {
		t.Fatalf("metadata=%v", values)
	}
	list, err := service.Library(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range list.Items {
		if item.ID == "378160973" {
			if item.Version != "4.2" || item.Parser != "" {
				t.Fatalf("list executed configuration parser: %#v", item)
			}
			return
		}
	}
	t.Fatal("downloaded Mod missing from library")
}

func TestLiteralMetadataOmitsDynamicAssignments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modinfo.lua")
	if err := os.WriteFile(path, []byte(`name = "old"; name = ChooseTranslationTable({}); version = "1"`), 0o640); err != nil {
		t.Fatal(err)
	}
	values := literalModMetadata(path)
	if _, exists := values["name"]; exists || values["version"] != "1" {
		t.Fatalf("metadata=%v", values)
	}
}
