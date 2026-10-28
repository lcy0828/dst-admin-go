package playeractivity

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestEventFieldsLuaEmptyTableCompatibility(t *testing.T) {
	for _, wire := range []string{`{}`, `[]`, `[ ]`, `{"count":2}`, `null`} {
		var e Event
		if err := json.Unmarshal([]byte(`{"fields":`+wire+`}`), &e); err != nil {
			t.Fatalf("%s: %v", wire, err)
		}
		if strings.HasPrefix(wire, "[") {
			encoded, _ := json.Marshal(e.Fields)
			if string(encoded) != "{}" {
				t.Fatalf("empty Lua table was not normalized: %s", encoded)
			}
		}
	}
	for _, wire := range []string{`[1]`, `[{}]`, `false`, `""`, `123`, `[`} {
		var e Event
		if json.Unmarshal([]byte(`{"fields":`+wire+`}`), &e) == nil {
			t.Fatalf("invalid dictionary accepted: %s", wire)
		}
	}
}

func TestLegacyLuaFieldsBackfillSurvivesRestart(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 6)
	good := map[string][]byte{}
	for i := 1; i <= 6; i++ {
		name := fmt.Sprintf("activity-%02d.json", i)
		var v Segment
		_ = json.Unmarshal(src.files[name], &v)
		if i == 1 {
			v.Events[0].Kind = "player.joined"
		}
		if i == 6 {
			v.Events[0].Kind = "player.left"
		}
		v.Events[0].Fields = EventFields{}
		wire, _ := json.Marshal(v)
		good[name] = []byte(strings.ReplaceAll(string(wire), `"fields":{}`, `"fields":[]`))
		// Seed durable rejected segments as an older reader would have left.
		src.files[name] = []byte(strings.ReplaceAll(string(wire), `"fields":{}`, `"fields":[1]`))
	}
	clock := time.Now()
	s.store.now = func() time.Time { return clock }
	s.collect(context.Background(), "room", "world", Defaults())
	coverage, _ := s.store.coverage("room", "world")
	if coverage.PendingSegments != 6 {
		t.Fatalf("missing recovery state: %+v", coverage)
	}
	s = New(s.store, s.rooms, src, s.commander)
	for name, data := range good {
		src.files[name] = data
	}
	for i := 0; i < 7; i++ {
		clock = clock.Add(31 * time.Second)
		s.collect(context.Background(), "room", "world", Defaults())
	}
	page, err := s.Events("room", Query{UserID: "KU_TEST"})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, e := range page.Items {
		kinds[e.Kind]++
		if e.Kind == "collection.gap" && e.Fields["recovered"] != true {
			t.Fatalf("resolved gap still presented as missing: %+v", e)
		}
	}
	coverage, _ = s.store.coverage("room", "world")
	if !coverage.Complete || coverage.RecoveredSegments != 6 || kinds["player.joined"] != 1 || kinds["player.left"] != 1 || kinds["position"] != 4 {
		t.Fatalf("legacy backfill incomplete/duplicated: %+v %+v", coverage, kinds)
	}
}

func TestIdleManifestDoesNotNeedHeartbeat(t *testing.T) {
	s, src := testService(t)
	fixture(t, src, 1)
	var m Manifest
	_ = json.Unmarshal(src.files["activity-manifest.json"], &m)
	m.PlayerCount = 0
	m.UpdatedAtUnix = time.Now().Add(-time.Hour).Unix()
	src.files["activity-manifest.json"], _ = json.Marshal(m)
	s.collect(context.Background(), "room", "world", Defaults())
	status, err := s.Status("room")
	if err != nil || len(status.Worlds) != 1 || status.Worlds[0].State != "idle" {
		t.Fatalf("healthy idle world misclassified: %+v %v", status, err)
	}
	m.PlayerCount = 1
	src.files["activity-manifest.json"], _ = json.Marshal(m)
	s.collect(context.Background(), "room", "world", Defaults())
	status, _ = s.Status("room")
	if status.Worlds[0].State != "inactive" {
		t.Fatalf("stale active output not detected: %+v", status)
	}
}
