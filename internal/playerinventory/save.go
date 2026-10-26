package playerinventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dont/shared"
)

var userPattern = regexp.MustCompile(`^KU_[A-Za-z0-9_-]{1,125}$`)
var sessionPattern = regexp.MustCompile(`^[A-Fa-f0-9]{16}$`)
var snapshotPattern = regexp.MustCompile(`^[0-9]{10}$`)

func ValidRequest(q shared.PlayerInventoryRequest) bool {
	return userPattern.MatchString(q.PlayerID) && (q.Source == "save" || q.Source == "live" || q.Source == "auto") && (q.SourceID == "" || len(q.SourceID) == 64 && isHex(q.SourceID))
}
func isHex(s string) bool { _, err := hex.DecodeString(s); return err == nil }

func EncodedUserDirectory(id string) (string, error) {
	if !userPattern.MatchString(id) || len(id) != 11 {
		return "", failure("ID_MAPPING_UNSUPPORTED")
	}
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	const output = "0123456789ABCDEFGHIJKLMNOPQRSTUV"
	var bits uint64
	for _, c := range id[:2] + id[3:] {
		index := strings.IndexRune(alphabet, c)
		if index < 0 {
			return "", failure("ID_MAPPING_UNSUPPORTED")
		}
		bits = bits<<6 | uint64(index)
	}
	var result [12]byte
	for i := 11; i >= 0; i-- {
		result[i] = output[bits&31]
		bits >>= 5
	}
	return string(result[:]), nil
}

func baseResult(player, source string) shared.PlayerInventory {
	return shared.PlayerInventory{SchemaVersion: 1, PlayerID: player, Source: source, CapturedAt: time.Now().UTC(), TimeSource: "file_modified", Status: "complete", Coverage: "standard_components", Equipment: []shared.InventoryItem{}, Inventory: []shared.InventoryItem{}, Warnings: []string{}, LimitsReached: []string{}}
}

// ReadSave never writes or executes the save. OpenRoot anchors each checked
// directory so renames/symlink races cannot redirect a read outside that root.
func ReadSave(ctx context.Context, saveRoot, cluster, shard string, q shared.PlayerInventoryRequest) (shared.PlayerInventory, error) {
	result := baseResult(q.PlayerID, "save")
	if !ValidRequest(q) {
		return result, failure("INVALID_PLAYER_ID")
	}
	root, err := os.OpenRoot(saveRoot)
	if err != nil {
		return result, failure("PLAYER_SNAPSHOT_NOT_FOUND")
	}
	defer root.Close()
	room, err := openDirectory(root, cluster)
	if err != nil {
		return result, err
	}
	defer room.Close()
	world, err := openDirectory(room, shard)
	if err != nil {
		return result, err
	}
	defer world.Close()

	for attempt := 0; attempt < 2; attempt++ {
		result = baseResult(q.PlayerID, "save")
		session, indexHash, sessionInfo, err := selectedSession(ctx, world)
		if err != nil {
			return result, err
		}
		if !sessionPattern.MatchString(session) {
			return result, failure("PLAYER_SNAPSHOT_NOT_FOUND")
		}
		result.SessionID = session
		save, err := openDirectory(world, "save")
		if err != nil {
			return result, err
		}
		sessions, err := openDirectory(save, "session")
		save.Close()
		if err != nil {
			return result, err
		}
		selected, err := openDirectory(sessions, session)
		sessions.Close()
		if err != nil {
			return result, err
		}
		value, readErr := readPlayer(ctx, selected, cluster, shard, session, q, result)
		selected.Close()
		after, afterHash, afterInfo, indexErr := selectedSession(ctx, world)
		worldInfo, worldErr := world.Stat(".")
		worldPathInfo, worldPathErr := room.Lstat(shard)
		roomInfo, roomErr := room.Stat(".")
		roomPathInfo, roomPathErr := root.Lstat(cluster)
		if indexErr != nil || after != session || indexHash != afterHash || !os.SameFile(sessionInfo, afterInfo) || worldErr != nil || worldPathErr != nil || !os.SameFile(worldInfo, worldPathInfo) || roomErr != nil || roomPathErr != nil || !os.SameFile(roomInfo, roomPathInfo) {
			readErr = failure("SAVE_CHANGED")
		}
		if readErr != nil && Code(readErr) == "SAVE_CHANGED" && attempt == 0 {
			continue
		}
		return value, readErr
	}
	return result, failure("SAVE_CHANGED")
}

func openDirectory(root *os.Root, name string) (*os.Root, error) {
	if name == "" || name == "." || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00") {
		return nil, failure("SAVE_INVALID")
	}
	before, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return nil, failure("PLAYER_SNAPSHOT_NOT_FOUND")
	}
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, failure("SAVE_INVALID")
	}
	dir, err := root.OpenRoot(name)
	if err != nil {
		return nil, failure("SAVE_CHANGED")
	}
	after, err := dir.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		dir.Close()
		return nil, failure("SAVE_CHANGED")
	}
	return dir, nil
}

type savedCandidate struct {
	directory, name, id string
	info                os.FileInfo
}

func readPlayer(ctx context.Context, sessionRoot *os.Root, cluster, shard, session string, q shared.PlayerInventoryRequest, result shared.PlayerInventory) (shared.PlayerInventory, error) {
	dirs := []string{q.PlayerID + "_", q.PlayerID}
	if encoded, err := EncodedUserDirectory(q.PlayerID); err == nil {
		dirs = append(dirs, encoded)
	}
	var candidates []savedCandidate
	for _, name := range dirs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		dir, err := openDirectory(sessionRoot, name)
		if err != nil {
			if Code(err) == "PLAYER_SNAPSHOT_NOT_FOUND" {
				continue
			}
			return result, err
		}
		file, info, err := latest(dir)
		dir.Close()
		if err != nil {
			return result, err
		}
		if file == "" {
			continue
		}
		sum := sha256.Sum256([]byte(cluster + "/" + shard + "/" + session + "/" + name))
		candidates = append(candidates, savedCandidate{name, file, hex.EncodeToString(sum[:]), info})
	}
	if len(candidates) == 0 {
		if len(q.PlayerID) != 11 {
			return result, failure("ID_MAPPING_UNSUPPORTED")
		}
		return result, failure("PLAYER_SNAPSHOT_NOT_FOUND")
	}
	var selected *savedCandidate
	for i := range candidates {
		c := &candidates[i]
		modified := c.info.ModTime().UTC()
		result.Candidates = append(result.Candidates, shared.InventoryCandidate{SourceID: c.id, SessionID: session, FileModifiedAt: &modified})
		if q.SourceID == c.id {
			selected = c
		}
	}
	if q.SourceID == "" && len(candidates) == 1 {
		selected = &candidates[0]
	}
	if selected == nil {
		result.Code = "SOURCE_AMBIGUOUS"
		return result, &shared.InventoryError{Code: result.Code, Candidates: result.Candidates}
	}
	dir, err := openDirectory(sessionRoot, selected.directory)
	if err != nil {
		return result, err
	}
	defer dir.Close()
	data, info, err := readRegular(dir, selected.name)
	if err != nil {
		return result, err
	}
	if !sameFile(info, selected.info) {
		return result, failure("SAVE_CHANGED")
	}
	if err = normalizeCached(ctx, data, &result, q.Refresh); err != nil {
		return result, err
	}
	file, latestInfo, err := latest(dir)
	if err != nil || file != selected.name || !sameFile(latestInfo, info) {
		return result, failure("SAVE_CHANGED")
	}
	directoryInfo, directoryErr := dir.Stat(".")
	currentDirectory, currentErr := sessionRoot.Lstat(selected.directory)
	if directoryErr != nil || currentErr != nil || !os.SameFile(directoryInfo, currentDirectory) {
		return result, failure("SAVE_CHANGED")
	}
	modified := info.ModTime().UTC()
	result.FileModifiedAt = &modified
	result.SourceID = selected.id
	result.Candidates = nil
	return result, nil
}

func latest(dir *os.Root) (string, os.FileInfo, error) {
	f, err := dir.Open(".")
	if err != nil {
		return "", nil, failure("SAVE_INVALID")
	}
	defer f.Close()
	entries, err := f.ReadDir(4097)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", nil, failure("SAVE_INVALID")
	}
	if len(entries) > 4096 {
		return "", nil, failure("SAVE_LIMIT_EXCEEDED")
	}
	var latest string
	var number uint64
	var info os.FileInfo
	for _, entry := range entries {
		if !snapshotPattern.MatchString(entry.Name()) {
			continue
		}
		n, err := strconv.ParseUint(entry.Name(), 10, 64)
		if err != nil || n == 0 {
			continue
		}
		if n > number {
			latest = entry.Name()
			number = n
		}
	}
	if latest != "" {
		info, err = dir.Lstat(latest)
		if err != nil || !info.Mode().IsRegular() {
			return "", nil, failure("SAVE_INVALID")
		}
	}
	return latest, info, nil
}

func readRegular(root *os.Root, name string) ([]byte, os.FileInfo, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, nil, failure("SAVE_CHANGED")
	}
	if !before.Mode().IsRegular() || before.Size() < 1 {
		return nil, nil, failure("SAVE_INVALID")
	}
	if before.Size() > MaxInput {
		return nil, nil, failure("SAVE_LIMIT_EXCEEDED")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, nil, failure("SAVE_CHANGED")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !sameFile(before, opened) {
		return nil, nil, failure("SAVE_CHANGED")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxInput+1))
	if err != nil {
		return nil, nil, failure("SAVE_INVALID")
	}
	after, err := f.Stat()
	pathInfo, pathErr := root.Lstat(name)
	if err != nil || pathErr != nil || !sameFile(opened, after) || !sameFile(opened, pathInfo) || int64(len(data)) != opened.Size() {
		return nil, nil, failure("SAVE_CHANGED")
	}
	return data, after, nil
}

func sameFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && a.Mode() == b.Mode()
}
