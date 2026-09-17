package runtimefiles

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dont/shared"
)

func TestChatHistoryPreservesGenerationAcrossDSTRotation(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, "room", "Master")
	if err := os.MkdirAll(world, 0o700); err != nil {
		t.Fatal(err)
	}
	writeChatFixture(t, world, "Sun Aug 23 19:08:35 2026", "[00:06:03]: [Say] (KU_ONE) lcy: 123\n")
	before, err := ListChatLogGenerations(context.Background(), root, "room", "Master")
	if err != nil || len(before) != 1 || before[0].Archived {
		t.Fatalf("before generations=%#v err=%v", before, err)
	}
	oldID := before[0].ID

	chatSuffix := "2026-08-23-19-33-25"
	serverSuffix := "2026-08-23-19-33-24"
	chatArchive := filepath.Join(world, "backup", "server_chat_log")
	serverArchive := filepath.Join(world, "backup", "server_log")
	if err := os.MkdirAll(chatArchive, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(serverArchive, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(world, "server_chat_log.txt"), filepath.Join(chatArchive, "server_chat_log_"+chatSuffix+".txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(world, "server_log.txt"), filepath.Join(serverArchive, "server_log_"+serverSuffix+".txt")); err != nil {
		t.Fatal(err)
	}
	// The first chat line is intentionally identical. Generation identity must
	// still change because it includes the paired server startup marker.
	writeChatFixture(t, world, "Sun Aug 23 20:24:11 2026", "[00:06:03]: [Say] (KU_ONE) lcy: 123\n")
	after, err := ListChatLogGenerations(context.Background(), root, "room", "Master")
	if err != nil || len(after) != 2 {
		t.Fatalf("after generations=%#v err=%v", after, err)
	}
	if !after[0].Archived || after[0].ID != oldID || after[1].ID == oldID {
		t.Fatalf("rotation identity was not preserved: before=%#v after=%#v", before, after)
	}
	chunk, err := ReadChatLogGeneration(context.Background(), root, "room", "Master", shared.RuntimeChatLogRequest{
		GenerationID: oldID, Cursor: 0, MaxBytes: 1024, MaxLines: 10,
	})
	if err != nil || !chunk.Complete || len(chunk.Lines) != 1 || chunk.Lines[0].Text != "[00:06:03]: [Say] (KU_ONE) lcy: 123" {
		t.Fatalf("archive chunk=%#v err=%v", chunk, err)
	}
	if err := ValidateChatLogResult(shared.RuntimeChatLogRequest{GenerationID: oldID, Cursor: 0, MaxBytes: 1024, MaxLines: 10}, chunk); err != nil {
		t.Fatal(err)
	}
}

func TestChatHistoryUsesEstimatedArchiveTimeWithoutPairedServerLog(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "room", "Caves", "backup", "server_chat_log")
	if err := os.MkdirAll(archive, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "server_chat_log_2026-08-23-20-25-17.txt"
	if err := os.WriteFile(filepath.Join(archive, name), []byte("[00:00:01]: [Join Announcement] lcy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	generations, err := ListChatLogGenerations(context.Background(), root, "room", "Caves")
	if err != nil || len(generations) != 1 || !generations[0].StartedAtEstimated {
		t.Fatalf("generations=%#v err=%v", generations, err)
	}
	want, _ := time.ParseInLocation("2006-01-02-15-04-05", "2026-08-23-20-25-17", time.Local)
	if !generations[0].StartedAt.Equal(want.UTC()) {
		t.Fatalf("startedAt=%s want=%s", generations[0].StartedAt, want.UTC())
	}
}

func TestChatHistoryPreservesUnpairedGenerationAcrossRename(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, "room", "Master")
	if err := os.MkdirAll(world, 0o700); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(world, "server_chat_log.txt")
	if err := os.WriteFile(current, []byte("[00:00:01]: [Say] (KU_ONE) lcy: hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := ListChatLogGenerations(context.Background(), root, "room", "Master")
	if err != nil || len(before) != 1 {
		t.Fatalf("before=%#v err=%v", before, err)
	}
	archive := filepath.Join(world, "backup", "server_chat_log")
	if err := os.MkdirAll(archive, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(current, filepath.Join(archive, "server_chat_log_2026-08-23-20-25-17.txt")); err != nil {
		t.Fatal(err)
	}
	after, err := ListChatLogGenerations(context.Background(), root, "room", "Master")
	if err != nil || len(after) != 1 || after[0].ID != before[0].ID || !after[0].StartedAtEstimated {
		t.Fatalf("unpaired generation identity changed: before=%#v after=%#v err=%v", before, after, err)
	}
}

func TestChatHistoryRejectsSymlinkedArchiveDirectory(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, "room", "Master")
	if err := os.MkdirAll(filepath.Join(world, "backup"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(world, "backup", "server_chat_log")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := ListChatLogGenerations(context.Background(), root, "room", "Master"); err == nil {
		t.Fatal("expected unsafe archive directory error")
	}
}

func TestChatHistoryPaginatesLinesWithoutDroppingFinalArchiveLine(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, "room", "Master")
	chatArchive := filepath.Join(world, "backup", "server_chat_log")
	serverArchive := filepath.Join(world, "backup", "server_log")
	if err := os.MkdirAll(chatArchive, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(serverArchive, 0o700); err != nil {
		t.Fatal(err)
	}
	suffix := "2026-08-23-19-33-25"
	content := "[00:00:01]: [Say] (KU_ONE) lcy: one\n[00:00:02]: [Say] (KU_ONE) lcy: two\n[00:00:03]: [Say] (KU_ONE) lcy: three"
	if err := os.WriteFile(filepath.Join(chatArchive, "server_chat_log_"+suffix+".txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	server := "[00:00:00]: Current time: Sun Aug 23 19:08:35 2026\n"
	if err := os.WriteFile(filepath.Join(serverArchive, "server_log_"+suffix+".txt"), []byte(server), 0o600); err != nil {
		t.Fatal(err)
	}
	generations, err := ListChatLogGenerations(context.Background(), root, "room", "Master")
	if err != nil || len(generations) != 1 {
		t.Fatalf("generations=%#v err=%v", generations, err)
	}
	request := shared.RuntimeChatLogRequest{GenerationID: generations[0].ID, Cursor: 0, MaxBytes: 1024, MaxLines: 2}
	first, err := ReadChatLogGeneration(context.Background(), root, "room", "Master", request)
	if err != nil || len(first.Lines) != 2 || first.Complete {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	request.Cursor = first.Cursor
	second, err := ReadChatLogGeneration(context.Background(), root, "room", "Master", request)
	if err != nil || len(second.Lines) != 1 || second.Lines[0].Text != "[00:00:03]: [Say] (KU_ONE) lcy: three" || !second.Complete {
		t.Fatalf("second=%#v err=%v", second, err)
	}
}

func writeChatFixture(t *testing.T, world, currentTime, chat string) {
	t.Helper()
	server := "[00:00:00]: Starting Up\n[00:00:00]: Current time: " + currentTime + "\n"
	if err := os.WriteFile(filepath.Join(world, "server_log.txt"), []byte(server), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(world, "server_chat_log.txt"), []byte(chat), 0o600); err != nil {
		t.Fatal(err)
	}
}
