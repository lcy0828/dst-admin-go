package runtimefiles

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestComposeChatTimesSparseDaysAndAmbiguity(t *testing.T) {
	start := time.Date(2026, 9, 6, 15, 51, 17, 0, time.UTC)
	rows := []chatClockRow{{cursor: 1, seconds: 6 * 3600, label: "join announcement", name: "ql"}, {cursor: 2, seconds: 6*3600 + 7*60 + 28, label: "say"}, {cursor: 3, seconds: 8 * 3600, label: "leave announcement", name: "ql"}}
	anchors := []chatClockAnchor{{name: "ql", kind: "join", at: start.Add(4*24*time.Hour + 6*time.Hour - time.Second)}, {name: "ql", kind: "leave", at: start.Add(4*24*time.Hour + 8*time.Hour)}}
	got := composeChatTimes(rows, start, start.Add(6*24*time.Hour), anchors)
	want := start.Add(4*24*time.Hour + 6*time.Hour + 7*time.Minute + 28*time.Second)
	if got[2] == nil || !got[2].Equal(want) {
		t.Fatalf("sparse message=%v want=%s", got[2], want)
	}
	if got := composeChatTimes(rows, start, start.Add(6*24*time.Hour), nil); len(got) != 0 {
		t.Fatalf("invented dates without evidence: %v", got)
	}
	// Same names at the same clock on two days must remain ambiguous.
	anchors = append(anchors, chatClockAnchor{name: "ql", kind: "join", at: start.Add(5*24*time.Hour + 6*time.Hour)}, chatClockAnchor{name: "ql", kind: "leave", at: start.Add(5*24*time.Hour + 8*time.Hour)})
	if got := composeChatTimes(rows, start, start.Add(6*24*time.Hour), anchors); len(got) != 0 {
		t.Fatalf("same-name ambiguity lost: %v", got)
	}
	if got := composeChatTimes(rows, time.Time{}, start, nil); len(got) != 0 {
		t.Fatal("missing startup guessed")
	}
}

func TestComposeChatTimesRolloverAndUnwrappedHours(t *testing.T) {
	start := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	for _, rows := range [][]chatClockRow{
		{{cursor: 1, seconds: 86399}, {cursor: 2, seconds: 2}},
		{{cursor: 1, seconds: 86399}, {cursor: 2, seconds: 86402}},
	} {
		got := composeChatTimes(rows, start, start.Add(25*time.Hour), nil)
		if got[2] == nil || !got[2].Equal(start.Add(24*time.Hour+2*time.Second)) {
			t.Fatalf("rollover=%v", got)
		}
	}
}

func TestChatClockIndexResumesPartialLinesAndResetsOnRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server_log.txt")
	data := "[00:00:00]: Current time: Sun Sep 6 15:51:17 2026\n[23:59:59]: tick\n[00:00:01]: Client authenticated: (KU_ONE) ql\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	chatClockCache.Lock()
	defer chatClockCache.Unlock()
	var state *chatClockFile
	for i := 0; i < 100; i++ {
		budget := int64(7)
		s, ready, err := scanChatClockFile(context.Background(), path, "server", false, false, &budget)
		if err != nil {
			t.Fatal(err)
		}
		state = s
		if budget < 0 {
			t.Fatal("read budget exceeded")
		}
		if ready {
			break
		}
	}
	if len(state.anchors) != 1 || state.anchors[0].at.Day() != 7 {
		t.Fatalf("anchors=%v", state.anchors)
	}
	if err := os.WriteFile(path, []byte("[00:00:00]: Current time: Tue Sep 8 15:51:17 2026\n[00:00:01]: Client authenticated: (KU_TWO) lcy"), 0600); err != nil {
		t.Fatal(err)
	}
	budget := int64(1024)
	state, _, err := scanChatClockFile(context.Background(), path, "server", false, false, &budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.anchors) != 0 {
		t.Fatal("unfinished line was imported")
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString("\n")
	f.Close()
	budget = 1024
	state, _, err = scanChatClockFile(context.Background(), path, "server", false, false, &budget)
	if err != nil || len(state.anchors) != 1 || state.anchors[0].at.Day() != 8 || state.anchors[0].name != "lcy" {
		t.Fatalf("restart anchors=%v err=%v", state.anchors, err)
	}
}
