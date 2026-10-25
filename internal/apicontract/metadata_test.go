package apicontract

import (
	"slices"
	"testing"
)

func TestShippedToolMetadataCoverageAndSemantics(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range c.Operations() {
		if err := op.validateToolMetadata(); err != nil {
			t.Fatal(err)
		}
		if !op.Mutation() && (!op.ToolMetadata().Idempotent || op.ToolMetadata().Access != "read") {
			t.Fatalf("bad read hints: %s", op.ID)
		}
	}
	for id, tags := range map[string][]string{"addModToRoom": {"room", "mod"}, "announceToRoomPlayer": {"room", "player"}, "applyRoomAccessLists": {"room", "access"}, "createDistributedBackupSet": {"room", "backup"}, "testWorkshopNetworkConnection": {"mod", "system"}} {
		op, _ := c.Operation(id)
		for _, tag := range tags {
			if !slices.Contains(op.ToolMetadata().Tags, tag) {
				t.Fatalf("%s missing %s", id, tag)
			}
		}
	}
	for _, id := range []string{"deleteBackup", "restoreBackup", "killRoomPlayer", "executeRawRoomCommand"} {
		op, _ := c.Operation(id)
		m := op.ToolMetadata()
		if m.Risk != "high" || !m.Destructive || m.Access != "write" {
			t.Fatalf("%s=%+v", id, m)
		}
	}
	op, _ := c.Operation("searchMods")
	if !op.ToolMetadata().OpenWorld {
		t.Fatal("Steam search is closed-world")
	}
	op, _ = c.Operation("executeRawRoomCommand")
	if op.ToolMetadata().Runtime != "shard-ready" {
		t.Fatal("Lua requires ready shard")
	}
}
func TestFilteredSearchFiltersBeforeLimiting(t *testing.T) {
	c, _ := Default()
	items := c.SearchFiltered("", 12, MetadataFilter{Tags: []string{"backup"}, Access: "read"})
	if len(items) == 0 {
		t.Fatal("no backup reads")
	}
	for _, op := range items {
		m := op.ToolMetadata()
		if m.Access != "read" || !slices.Contains(m.Tags, "backup") {
			t.Fatal(op.ID, m)
		}
	}
	if len(c.SearchFiltered("", 12, MetadataFilter{Tags: []string{"unknown-tag"}})) != 0 {
		t.Fatal("unknown tag returned unrelated tools")
	}
}
