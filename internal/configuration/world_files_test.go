package configuration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dont/shared"
)

func TestWorldFilesPasteClientSavePreservesCommentsAndRootFields(t *testing.T) {
	service, roomPath, backup := newConfigurationService(t)

	files, err := service.WorldFiles("room", "world")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := service.WorldConfig("room", "world")
	if err != nil {
		t.Fatal(err)
	}
	if files.Revision != cfg.Revision || files.WorldType != "forest" {
		t.Fatalf("WorldFiles mismatch: files=%#v cfg=%#v", files, cfg)
	}

	pastedLua := "\xEF\xBB\xBFKLEI     1 return {\r\n" +
		"  -- keep my custom comment\r\n" +
		"  location = \"forest\",\r\n" +
		"  worldgen_preset = \"ENDLESS\",\r\n" +
		"  substitutes = {},\r\n" +
		"  overrides = {\r\n" +
		"    day = \"longday\",\r\n" +
		"    custom_mod_worldopt = \"enabled\",\r\n" +
		"  },\r\n" +
		"}\r\n"

	preview, err := service.PreviewWorldFiles("room", "world", WorldFilesUpdateRequest{
		ExpectedRevision:  files.Revision,
		Leveldataoverride: &pastedLua,
	})
	if err != nil {
		t.Fatalf("PreviewWorldFiles failed: %v", err)
	}
	if strings.HasPrefix(preview.Leveldataoverride, "KLEI") {
		t.Fatalf("KLEI header was not stripped:\n%s", preview.Leveldataoverride)
	}
	if !strings.Contains(preview.Leveldataoverride, "-- keep my custom comment") {
		t.Fatalf("Lua comment was stripped despite Format=false:\n%s", preview.Leveldataoverride)
	}
	if preview.Overrides["day"] != "longday" || preview.Overrides["custom_mod_worldopt"] != "enabled" || preview.UnknownFieldCount != 1 {
		t.Fatalf("parsed overrides mismatch: %#v (unknown=%d)", preview.Overrides, preview.UnknownFieldCount)
	}
	foundHeaderNotice := false
	for _, notice := range preview.Notices {
		if notice == "stripped_klei_header" {
			foundHeaderNotice = true
		}
	}
	if !foundHeaderNotice {
		t.Fatalf("expected stripped_klei_header notice, got %v", preview.Notices)
	}

	result, err := service.ApplyWorldFiles(context.Background(), "job-world-files-1", "room", "world", WorldFilesUpdateRequest{
		ExpectedRevision:  files.Revision,
		Leveldataoverride: &pastedLua,
	})
	if err != nil {
		t.Fatalf("ApplyWorldFiles failed: %v", err)
	}
	if result.Revision != preview.NextRevision || backup.count != 0 {
		t.Fatalf("unexpected apply result: %#v (backups=%d)", result, backup.count)
	}

	writtenLua, err := os.ReadFile(filepath.Join(roomPath, "Master", "leveldataoverride.lua"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(writtenLua), "-- keep my custom comment") || !strings.Contains(string(writtenLua), `worldgen_preset = "ENDLESS"`) {
		t.Fatalf("disk leveldataoverride.lua lost comment or root preset:\n%s", writtenLua)
	}

	// Subsequent visual form patch over pasted file keeps root-level worldgen_preset
	patchedPreview, err := service.PreviewWorldFiles("room", "world", WorldFilesUpdateRequest{
		ExpectedRevision: result.Revision,
		OverridePatch: map[string]json.RawMessage{
			"autumn": json.RawMessage(`"longseason"`),
		},
	})
	if err != nil {
		t.Fatalf("PreviewWorldFiles with OverridePatch failed: %v", err)
	}
	if !strings.Contains(patchedPreview.Leveldataoverride, `worldgen_preset = "ENDLESS"`) || patchedPreview.Overrides["autumn"] != "longseason" {
		t.Fatalf("OverridePatch lost root preset or failed to apply:\n%s", patchedPreview.Leveldataoverride)
	}
}

func TestWorldFilesGuardrailsLocationMismatchIsMasterAndModOverridesPaste(t *testing.T) {
	service, _, _ := newConfigurationService(t)
	files, err := service.WorldFiles("room", "world")
	if err != nil {
		t.Fatal(err)
	}

	// 1. Cave config pasted into Forest world
	caveLua := `return { location = "cave", overrides = { world_size = "medium" } }`
	_, err = service.PreviewWorldFiles("room", "world", WorldFilesUpdateRequest{
		ExpectedRevision:  files.Revision,
		Leveldataoverride: &caveLua,
	})
	var locErr *WorldLocationMismatchError
	if !errors.As(err, &locErr) || locErr.Expected != "forest" || locErr.Actual != "cave" {
		t.Fatalf("expected WorldLocationMismatchError(forest vs cave), got %v", err)
	}

	// 2. Accidental modoverrides.lua pasted into WorldSettings
	modLua := `return { ["workshop-378160973"] = { enabled = true } }`
	_, err = service.PreviewWorldFiles("room", "world", WorldFilesUpdateRequest{
		ExpectedRevision:  files.Revision,
		Leveldataoverride: &modLua,
	})
	var synErr *FileSyntaxError
	if !errors.As(err, &synErr) || synErr.File != "leveldataoverride.lua" || !strings.Contains(synErr.Message, "workshop-*") {
		t.Fatalf("expected helpful modoverrides paste error, got %v", err)
	}

	// 3. Syntax error reports exact line
	brokenLua := "return {\n  location = \"forest\",\n  overrides = {\n    day == \"default\"\n  }\n}\n"
	_, err = service.PreviewWorldFiles("room", "world", WorldFilesUpdateRequest{
		ExpectedRevision:  files.Revision,
		Leveldataoverride: &brokenLua,
	})
	if !errors.As(err, &synErr) || synErr.File != "leveldataoverride.lua" || synErr.Line != 4 {
		t.Fatalf("expected syntax error at line 4, got %#v (%v)", synErr, err)
	}

	// 4. Flipping SHARD.is_master in server.ini is blocked
	flippedIni := strings.Replace(files.ServerIni, "is_master = true", "is_master = false", 1)
	_, err = service.PreviewWorldFiles("room", "world", WorldFilesUpdateRequest{
		ExpectedRevision: files.Revision,
		ServerIni:        &flippedIni,
	})
	var fieldErr *FieldError
	if !errors.As(err, &fieldErr) || fieldErr.Fields["server.isMaster"] == "" {
		t.Fatalf("expected server.isMaster FieldError, got %v", err)
	}
}

func TestWorldFilesRoutedAgentPublicationUsesExpectedDigests(t *testing.T) {
	service, roomPath, _ := newConfigurationService(t)
	publisher := &configurationPublisher{}
	if err := service.ConfigurePublisher(publisher); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	remoteServer := "[NETWORK]\nserver_port = 11000\n[SHARD]\nis_master = true\nname = Master\nid = 1\n[STEAM]\nauthentication_port = 8769\nmaster_server_port = 27019\n[ACCOUNT]\nencode_user_path = true\n"
	remoteLua := "return { location = \"forest\", overrides = { day = \"default\" } }\n"
	reader := configurationSnapshotReader{files: []shared.RuntimeConfigurationFile{
		routedConfigurationFile("server.ini", remoteServer, 0o640, now),
		routedConfigurationFile("leveldataoverride.lua", remoteLua, 0o640, now),
	}}
	if err := service.ConfigureReader(reader); err != nil {
		t.Fatal(err)
	}

	current, err := service.WorldFiles("room", "world")
	if err != nil {
		t.Fatal(err)
	}
	nextLua := "return { location = \"forest\", overrides = { day = \"onlyday\" } }\n"
	result, err := service.ApplyWorldFiles(context.Background(), "job-remote-files", "room", "world", WorldFilesUpdateRequest{
		ExpectedRevision:  current.Revision,
		Leveldataoverride: &nextLua,
	})
	if err != nil {
		t.Fatalf("routed ApplyWorldFiles failed: %v", err)
	}
	if result.PublishedTargets != 1 || len(publisher.requests) != 1 {
		t.Fatalf("expected 1 remote publication, got result=%#v requests=%#v", result, publisher.requests)
	}
	pub := publisher.requests[0]
	if !pub.IncludeLocal || pub.ExpectedFiles["server.ini"] == "" || pub.ExpectedFiles["leveldataoverride.lua"] == "" {
		t.Fatalf("expected ExpectedFiles SHA-256 digests on routed publication, got %#v", pub)
	}
	localLua, err := os.ReadFile(filepath.Join(roomPath, "Master", "leveldataoverride.lua"))
	if err != nil || strings.Contains(string(localLua), "onlyday") {
		t.Fatalf("routed ApplyWorldFiles unexpectedly mutated local controller disk: %v\n%s", err, localLua)
	}
}
