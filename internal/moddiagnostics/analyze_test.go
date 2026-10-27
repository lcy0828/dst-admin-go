package moddiagnostics

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"dont/internal/logstream"
)

func logLines(text ...string) []logstream.Line {
	lines := make([]logstream.Line, len(text))
	for i, s := range text {
		lines[i] = logstream.Line{Cursor: int64(i + 10), Text: s}
	}
	return lines
}

func TestStackContextIsSuspectedInvolvementNotBlame(t *testing.T) {
	start := time.Now().UTC()
	r := Analyze(Input{Observation: Observation{Mods: []Mod{{ID: "100"}, {ID: "200"}}}, IncludeLogs: true, LatestStart: &start,
		Logs: &logstream.Snapshot{StartedAt: start.Add(time.Second), Lines: logLines(
			`Loading mod ../mods/workshop-999/modmain.lua`,
			`LUA ERROR stack traceback:`,
			`../mods/workshop-100/modmain.lua:20: in function 'fn'`,
			`../mods/workshop-200/scripts/patch.lua:50: in function 'OnLoad'`,
			`../mods/my_custom_mod/modmain.lua:42: in main chunk`,
		)}})
	if r.LogCorrelation != "matched" || len(r.Evidence) != 1 || !slices.Equal(r.Evidence[0].ModIDs, []string{"100", "200", "local:my_custom_mod"}) {
		t.Fatalf("lost stack or misattributed routine message: %+v", r)
	}
	if !slices.Contains(r.Limitations, "STACK_REFERENCE_IS_NOT_CAUSATION") {
		t.Fatal("missing causal limitation")
	}
	for _, f := range r.Findings {
		if f.Code == "MOD_IN_ERROR_STACK" && f.Kind != "suspected" {
			t.Fatal("a stack frame became proof", f)
		}
	}
	if len(r.Evidence[0].Lines) != 5 {
		t.Fatal("context was filtered to error lines")
	}
}

func TestOldLogNeverExplainsNewStartAndMissingLogIsUnknown(t *testing.T) {
	start := time.Now().UTC()
	r := Analyze(Input{IncludeLogs: true, LatestStart: &start, Logs: &logstream.Snapshot{StartedAt: start.Add(-3 * time.Second), Lines: logLines(`LUA ERROR ../mods/workshop-1/modmain.lua:1: attempt to index a nil value`)}})
	if r.LogCorrelation != "historical" || len(r.Findings) != 0 || len(r.Evidence) != 0 || !slices.Contains(r.Limitations, "LOG_FROM_PREVIOUS_START") {
		t.Fatal(r)
	}
	r = Analyze(Input{IncludeLogs: true})
	if r.Status != "insufficient_evidence" || !slices.Contains(r.Limitations, "LOG_UNAVAILABLE") {
		t.Fatal(r)
	}
	r = Analyze(Input{IncludeLogs: true, Logs: &logstream.Snapshot{Truncated: true, Lines: logLines("Saving...")}})
	if r.LogCorrelation != "unknown" || !slices.Contains(r.Limitations, "LOG_TAIL_ONLY") || len(r.Findings) > 0 {
		t.Fatal(r)
	}
}

func TestErrorsAreBoundedAndCredentialsMasked(t *testing.T) {
	var lines []string
	for i := 0; i < 8; i++ {
		lines = append(lines, fmt.Sprintf("LUA ERROR number %d", i), `token = TOP_SECRET`, `password='PASSWORD'`, `player KU_123456 76561198999999999 192.0.2.35`)
		for j := 0; j < 50; j++ {
			lines = append(lines, strings.Repeat("x", 1500))
		}
	}
	r := Analyze(Input{IncludeLogs: true, Logs: &logstream.Snapshot{Lines: logLines(lines...)}})
	if len(r.Evidence) != MaxEvidence {
		t.Fatalf("evidence cap: %d", len(r.Evidence))
	}
	for _, e := range r.Evidence {
		if len(e.Lines) > MaxContextLines || !e.Truncated {
			t.Fatal("unbounded stack", e)
		}
		all := strings.Join(e.Lines, "\n")
		for _, secret := range []string{"TOP_SECRET", "PASSWORD", "KU_123456", "76561198999999999", "192.0.2.35"} {
			if strings.Contains(all, secret) {
				t.Fatal("sensitive log value leaked", secret)
			}
		}
	}
}

func TestVersionDomainsAndIncompleteModComparison(t *testing.T) {
	b := &Baseline{ReleaseID: "update-1", Observation: Observation{TargetID: "agent:a", InstallationID: "one", Version: Version{AppID: "343050", SteamBuild: "100", GameVersion: "750000"}, ConfigurationRevision: "before", Mods: []Mod{{ID: "1", Version: "1.0"}, {ID: "2"}}}}
	o := Observation{TargetID: "agent:a", InstallationID: "one", Version: Version{AppID: "343050", SteamBuild: "101", GameVersion: "751000"}, ConfigurationRevision: "after", Mods: []Mod{{ID: "1", Version: "2.0"}, {ID: "3"}}}
	r := Analyze(Input{Observation: o, Baseline: b})
	if len(r.Changes) != 6 {
		t.Fatalf("changes=%+v", r.Changes)
	}
	o.Limitations = []string{"MOD_FACTS_UNAVAILABLE"}
	o.Mods = nil
	o.Version.AppID = "322330"
	r = Analyze(Input{Observation: o, Baseline: b})
	if len(r.Changes) != 1 || r.Changes[0].Field != "configurationRevision" || !slices.Contains(r.Limitations, "VERSION_NOT_COMPARABLE") || !slices.Contains(r.Limitations, "MOD_COMPARISON_INCOMPLETE") {
		t.Fatal(r)
	}
	o.TargetID = "local"
	r = Analyze(Input{Observation: o, Baseline: b})
	if len(r.Changes) != 0 || !slices.Contains(r.Limitations, "BASELINE_PLACEMENT_CHANGED") {
		t.Fatal(r)
	}
}

func TestDeclarationsAndMissingFilesAreFactsNotVersionGuarantees(t *testing.T) {
	no := false
	r := Analyze(Input{Observation: Observation{Mods: []Mod{{ID: "1", DSTCompatible: &no, FileStatus: "invalid", Dependencies: []string{"2"}}}}})
	if r.Status != "observed_problem" || len(r.Findings) != 3 {
		t.Fatal(r)
	}
	yes := true
	r = Analyze(Input{Observation: Observation{Mods: []Mod{{ID: "1", DSTCompatible: &yes, Version: "latest", FileStatus: "present"}}}})
	if len(r.Findings) != 0 || !slices.Contains(r.Limitations, "NOT_A_COMPATIBILITY_GUARANTEE") {
		t.Fatal(r)
	}
}

func TestRemoteCommandEchoDoesNotCreateFalseError(t *testing.T) {
	r := Analyze(Input{IncludeLogs: true, Logs: &logstream.Snapshot{Lines: logLines(`RemoteCommandInput: "print('LUA ERROR')"`, `Normal output`)}})
	if len(r.Findings) != 0 {
		t.Fatal(r)
	}
}

func TestLuaMessageAndStackStayInTheSameErrorBlock(t *testing.T) {
	r := Analyze(Input{IncludeLogs: true, Logs: &logstream.Snapshot{Lines: logLines(
		"LUA ERROR stack traceback:",
		`[string "../mods/workshop-123/modmain.lua"]:12: attempt to index global 'missing' (a nil value)`,
		`[string "../mods/workshop-456/scripts/common.lua"]:30: in function 'Update'`,
	)}})
	if len(r.Evidence) != 1 || !slices.Equal(r.Evidence[0].ModIDs, []string{"123", "456"}) {
		t.Fatal("one error was split and lost its causal context", r.Evidence)
	}
}

func BenchmarkAnalyzeThousandLineTail(b *testing.B) {
	lines := make([]logstream.Line, 1000)
	for i := range lines {
		lines[i] = logstream.Line{Cursor: int64(i), Text: "[00:01:00]: Serializing world and entities..."}
	}
	lines[900].Text = "LUA ERROR stack traceback:"
	lines[901].Text = "../mods/workshop-123/modmain.lua:40: in function OnLoad"
	in := Input{IncludeLogs: true, Logs: &logstream.Snapshot{Lines: lines}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Analyze(in)
	}
}
