package servicelogs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dont/shared"
)

func TestPersistentRotationPaginationContextAndRestart(t *testing.T) {
	root := t.TempDir()
	s, err := Open(Config{Root: root, Component: "controller", Version: "1.0.0", FileBytes: 900})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		s.Append(shared.ServiceLogEntry{Message: fmt.Sprintf("message-%03d", i), JobID: "job-1"})
	}
	s.Close()
	q := shared.ServiceLogQuery{JobID: "job-1", Limit: 7}
	seen := map[string]bool{}
	var anchor shared.ServiceLogEntry
	for i := 0; i < 20; i++ {
		p, err := Query(context.Background(), root, q)
		if err != nil || p.CoverageLost {
			t.Fatalf("page: %+v %v", p, err)
		}
		for _, entry := range p.Items {
			if seen[entry.ID] {
				t.Fatalf("duplicate %s", entry.ID)
			}
			if entry.Message != fmt.Sprintf("message-%03d", 49-len(seen)) {
				t.Fatalf("order: %s at %d", entry.Message, len(seen))
			}
			seen[entry.ID] = true
			anchor = entry
		}
		if !p.HasMore {
			break
		}
		q.Cursor = p.NextCursor
	}
	if len(seen) != 50 {
		t.Fatal("missing", len(seen))
	}
	p, err := Context(context.Background(), root, anchor.ID, 3)
	if err != nil || len(p.Items) == 0 {
		t.Fatal(p, err)
	}
	q.Level = "error"
	if _, err := Query(context.Background(), root, q); !errors.Is(err, ErrInvalid) {
		t.Fatal("changed cursor filters accepted", err)
	}
	s, err = Open(Config{Root: root, Component: "controller"})
	if err != nil {
		t.Fatal(err)
	}
	s.Append(shared.ServiceLogEntry{Message: "after restart"})
	s.Close()
	p, err = Query(context.Background(), root, shared.ServiceLogQuery{})
	if err != nil || len(p.Items) != 51 || p.Items[0].Message != "after restart" {
		t.Fatal(len(p.Items), err)
	}
	files, _ := logFiles(root)
	for _, f := range files {
		os.Remove(filepath.Join(root, f.Name))
	}
	if _, err := Context(context.Background(), root, anchor.ID, 3); !errors.Is(err, ErrCursorExpired) {
		t.Fatal(err)
	}
}

func TestRedactionAndOversizedEscapedRecordsRemainReadable(t *testing.T) {
	root := t.TempDir()
	s, err := Open(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	s.Append(shared.ServiceLogEntry{Message: strings.Repeat("\x00\"\\", 16000) + ` api_key="private-value" Authorization: Bearer private-bearer`, Stack: strings.Repeat("\x00", 8192)})
	s.Close()
	p, err := Query(context.Background(), root, shared.ServiceLogQuery{})
	if err != nil || p.CoverageLost || len(p.Items) != 1 || !p.Items[0].Truncated {
		t.Fatal(len(p.Items), p.CoverageLost, err)
	}
	files, _ := logFiles(root)
	data, _ := os.ReadFile(filepath.Join(root, files[0].Name))
	if len(data) > maxEntryBytes {
		t.Fatal("oversized serialized record", len(data))
	}
	for _, raw := range []string{`{"Authorization":"Bearer private-bearer"}`, `Cookie: private-cookie`, `api_key="private-value"`, `https://user:private-pass@host/path?token=private-token`, `pds-g^private-klei-token`, `sk-private-openai-key-1234567890`} {
		redacted := Redact(raw)
		if strings.Contains(redacted, "private-") {
			t.Fatalf("secret persisted: %s", redacted)
		}
	}
}

func TestSettingsConflictDisabledConsoleAndWriterExclusion(t *testing.T) {
	root := t.TempDir()
	settings := DefaultSettings()
	settings.Enabled = false
	if _, err := SaveSettings(root, settings); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveSettings(root, settings); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	settings = ReadSettings(root)
	future := time.Now().Add(3 * time.Hour)
	settings.DebugUntil = &future
	if _, err := SaveSettings(root, settings); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	var console bytes.Buffer
	s, err := Open(Config{Root: root, Console: &console})
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Open(Config{Root: root}); err == nil {
		second.Close()
		t.Fatal("second writer accepted")
	}
	s.Append(shared.ServiceLogEntry{Message: "still visible on terminal"})
	s.Close()
	if !strings.Contains(console.String(), "still visible on terminal") {
		t.Fatal(console.String())
	}
	files, _ := logFiles(root)
	if len(files) != 0 {
		t.Fatal("disabled still persisted", files)
	}
	if Health(root).Persistence != "disabled" {
		t.Fatal(Health(root))
	}
}

func TestRefreshAnchorsAndInvalidPaths(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(Config{Root: root})
	s.Append(shared.ServiceLogEntry{Message: "old"})
	s.Append(shared.ServiceLogEntry{Message: "new"})
	s.Close()
	p, _ := Query(context.Background(), root, shared.ServiceLogQuery{})
	fresh, err := Query(context.Background(), root, shared.ServiceLogQuery{AfterID: p.Items[1].ID})
	if err != nil || fresh.CoverageLost || len(fresh.Items) != 1 {
		t.Fatal(fresh, err)
	}
	fresh, err = Query(context.Background(), root, shared.ServiceLogQuery{AfterID: "missing"})
	if err != nil || !fresh.CoverageLost {
		t.Fatal(fresh, err)
	}
	for _, q := range []shared.ServiceLogQuery{{Limit: 501}, {Level: "bogus"}, {Cursor: "../outside"}, {From: time.Now().Add(-32 * 24 * time.Hour)}, {JobID: "bad\nvalue"}} {
		if _, err := Query(context.Background(), root, q); !errors.Is(err, ErrInvalid) {
			t.Fatal(q, err)
		}
	}
	if _, err := Context(context.Background(), root, "Li4vYXBwLmNvbmZAMA", 10); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestConcurrentAppendsBoundMemoryAndReportDrops(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(Config{Root: root})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				s.Append(shared.ServiceLogEntry{Message: fmt.Sprintf("event-%d", j)})
			}
		}()
	}
	wg.Wait()
	s.Close()
	if s.Health().Dropped == 0 {
		t.Log("writer kept up; no dropped records")
	}
	data, _ := json.Marshal(s.Health())
	if !json.Valid(data) {
		t.Fatal(string(data))
	}
}

func TestChildOutputBoundsAndRecoversRawCrashOutput(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(Config{Root: root})
	w := &ChildWriter{component: "agent", boot: "boot", store: s}
	_, _ = w.Write([]byte("panic: boom\ngoroutine 1 [running]:\nstack frame\n"))
	_, _ = w.Write([]byte(strings.Repeat("x", 200000)))
	w.Flush()
	s.Close()
	p, err := Query(context.Background(), root, shared.ServiceLogQuery{})
	if err != nil || p.CoverageLost || len(p.Items) < 4 {
		t.Fatal(len(p.Items), err)
	}
	found := false
	for _, v := range p.Items {
		if v.Message == "panic: boom" && v.Level == "error" && v.Component == "agent" {
			found = true
		}
	}
	if !found {
		t.Fatal("lost crash output")
	}
}

func TestPersistenceFailureBacksOffWithoutLosingTerminalOutput(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "logs")
	var console bytes.Buffer
	s, err := Open(Config{Root: root, Console: &console})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, filepath.Join(base, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		s.Append(shared.ServiceLogEntry{Message: fmt.Sprintf("failure-event-%d", i)})
	}
	s.Close()
	h := s.Health()
	if h.Persistence != "degraded" || h.WriteErrors != 1 || h.Dropped != 50 {
		t.Fatal(h)
	}
	if !strings.Contains(console.String(), "failure-event-49") || strings.Count(console.String(), "persistence failed") != 1 {
		t.Fatal(console.String())
	}
}

func TestRetentionRejectsExpiredRecordsEvenInAnActiveSegment(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(Config{Root: root})
	s.Append(shared.ServiceLogEntry{Time: time.Now().Add(-8 * 24 * time.Hour), Message: "expired"})
	s.Append(shared.ServiceLogEntry{Message: "current"})
	s.Close()
	p, err := Query(context.Background(), root, shared.ServiceLogQuery{From: time.Now().Add(-10 * 24 * time.Hour)})
	if err != nil || len(p.Items) != 1 || p.Items[0].Message != "current" || !p.CoverageLost {
		t.Fatal(p, err)
	}
}

func BenchmarkServiceLogQueries(b *testing.B) {
	root := b.TempDir()
	file, err := os.Create(filepath.Join(root, "service-00000000000000000001-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.jsonl"))
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 10000; i++ {
		data, _ := json.Marshal(shared.ServiceLogEntry{ID: fmt.Sprintf("id-%d", i), Time: time.Now(), Level: "info", Component: "controller", Message: strings.Repeat("normal diagnostic record ", 40)})
		file.Write(append(data, '\n'))
	}
	file.Close()
	b.Run("latest100", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := Query(context.Background(), root, shared.ServiceLogQuery{}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("boundedMissingKeyword", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			p, err := Query(context.Background(), root, shared.ServiceLogQuery{Keyword: "no-such-error"})
			if err != nil || !p.Limited || p.ScannedBytes > scanBudget+65536 {
				b.Fatal(p.ScannedBytes, err)
			}
		}
	})
	b.Run("sourceStatus", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := Source(root); err != nil {
				b.Fatal(err)
			}
		}
	})
}
