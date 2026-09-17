package mods

import "testing"

func TestRoomProfileConfigurationDifferencesIgnoreEnabledState(t *testing.T) {
	profile, err := DeriveRoomModProfile("room", []RoomProfileWorldInput{
		{WorldID: "master", Master: true, Content: []byte(`return {["workshop-100"]={enabled=true,configuration_options={mode="easy"}}}`)},
		{WorldID: "caves", Content: []byte(`return {["workshop-100"]={enabled=false,configuration_options={mode="easy"}}}`)},
	})
	if err != nil { t.Fatal(err) }
	if profile.Items[0].ConfigurationMixed { t.Fatal("enabled state was treated as a configuration difference") }
}

func TestDeriveRoomModProfileUsesMasterAsDefaultAndFindsExceptions(t *testing.T) {
	profile, err := DeriveRoomModProfile("room-1", []RoomProfileWorldInput{
		{WorldID: "caves", Name: "Caves", Content: []byte(`return {
  ["workshop-100"] = { enabled = false, configuration_options = { mode = "hard" } },
  ["workshop-200"] = { enabled = true, configuration_options = {} },
}`)},
		{WorldID: "master", Name: "Master", Master: true, Content: []byte(`return {
  ["workshop-100"] = { configuration_options = { mode = "easy" }, enabled = true },
}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if profile.DefaultWorldID != "master" || len(profile.Worlds) != 2 || profile.Worlds[0].WorldID != "master" {
		t.Fatalf("unexpected default world: %#v", profile)
	}
	if len(profile.Items) != 2 {
		t.Fatalf("unexpected items: %#v", profile.Items)
	}
	first := profile.Items[0]
	if first.ModID != "100" || !first.DefaultConfigured || !first.DefaultEnabled || len(first.InheritedWorldIDs) != 1 || first.InheritedWorldIDs[0] != "master" || len(first.ExceptionWorldIDs) != 1 || first.ExceptionWorldIDs[0] != "caves" {
		t.Fatalf("unexpected first profile item: %#v", first)
	}
	second := profile.Items[1]
	if second.ModID != "200" || second.DefaultConfigured || len(second.ExceptionWorldIDs) != 1 || second.Worlds[1].Difference != "configured" {
		t.Fatalf("unexpected second profile item: %#v", second)
	}
}

func TestDeriveRoomModProfileIgnoresLuaFormattingDifferences(t *testing.T) {
	profile, err := DeriveRoomModProfile("room-1", []RoomProfileWorldInput{
		{WorldID: "master", Name: "Master", Master: true, Content: []byte(`return {["workshop-100"]={enabled=true,configuration_options={mode="easy"}}}`)},
		{WorldID: "caves", Name: "Caves", Content: []byte(`return {
  ["workshop-100"] = {
    configuration_options = { mode = "easy" },
    enabled = true,
  },
}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(profile.Items) != 1 || len(profile.Items[0].ExceptionWorldIDs) != 0 || len(profile.Items[0].InheritedWorldIDs) != 2 {
		t.Fatalf("formatting was treated as an override: %#v", profile.Items)
	}
}
