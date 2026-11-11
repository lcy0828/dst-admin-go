package dstruntime

import (
	"dont/internal/landmarks"
	"encoding/json"
	"testing"
)

func TestLandmarkQueryReadsActualEntitiesAndMoonIslandTopology(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "baths", true: "old world"}[fallback], func(t *testing.T) {
			L, out := queryLuaState(t, 0)
			defer L.Close()
			id := "MoonIsland_Baths"
			if fallback {
				id = "MoonIsland_Forest"
			}
			if err := L.DoString(`os.clock=function()return 0 end
local function entity(prefab,x,z) return {prefab=prefab,Transform={GetWorldPosition=function()return x,0,z end},IsValid=function()return true end}end
Ents={entity("pigking",12,24),entity("multiplayer_portal",36,48),entity("moonbase",999,999),entity("oasislake",60,72)}
TheWorld.topology={ids={"` + id + `","MoonIsland_Beach"},nodes={{x=84,y=96},{x=100,y=110}}}
`); err != nil {
				t.Fatal(err)
			}
			if err := L.DoString(queryScript(t, "landmarks", "")); err != nil {
				t.Fatal(err)
			}
			var result struct {
				Complete bool           `json:"complete"`
				Data     landmarks.Data `json:"data"`
			}
			if err := json.Unmarshal([]byte(*out), &result); err != nil || !result.Complete || result.Data.Count != 4 {
				t.Fatal(*out, err)
			}
			found := false
			for _, p := range result.Data.Items {
				if !p.Valid() {
					t.Fatal(p)
				}
				if p.LandmarkID == "lunar_island" {
					found = p.X == 84 && p.Z == 96
				}
			}
			if !found {
				t.Fatal("wrong lunar island coordinates", result)
			}
		})
	}
}
func TestLandmarkQueryIsBoundedAndMissingDataNeverFabricatesPoints(t *testing.T) {
	for _, setup := range []string{`Ents=nil`} {
		L, out := queryLuaState(t, 0)
		if err := L.DoString(setup); err != nil {
			t.Fatal(err)
		}
		err := L.DoString(queryScript(t, "landmarks", ""))
		if setup == `Ents=nil` && err == nil {
			t.Fatal("missing entities returned success", *out)
		}
		L.Close()
	}
	Lcpu, cpuOut := queryLuaState(t, 1000)
	defer Lcpu.Close()
	if err := Lcpu.DoString(`local n=0;os.clock=function()n=n+.01;return n end`); err != nil {
		t.Fatal(err)
	}
	if err := Lcpu.DoString(queryScript(t, "landmarks", "")); err != nil {
		t.Fatal(err)
	}
	var cpu struct {
		Complete bool   `json:"complete"`
		Reason   string `json:"reason"`
	}
	if json.Unmarshal([]byte(*cpuOut), &cpu) != nil || cpu.Complete || cpu.Reason != "CPU_BUDGET" {
		t.Fatal(*cpuOut)
	}
	L, out := queryLuaState(t, 100001)
	defer L.Close()
	if err := L.DoString(`os.clock=function()return 0 end`); err != nil {
		t.Fatal(err)
	}
	if err := L.DoString(queryScript(t, "landmarks", "")); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Complete bool   `json:"complete"`
		Reason   string `json:"reason"`
	}
	if json.Unmarshal([]byte(*out), &result) != nil || result.Complete || result.Reason != "SCAN_LIMIT" {
		t.Fatal(*out)
	}
}
