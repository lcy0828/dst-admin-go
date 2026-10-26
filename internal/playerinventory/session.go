package playerinventory

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
)

// Read the current session through the same anchored, bounded data reader as
// player snapshots. Re-read the index bytes on each query, even when timestamps
// are preserved by a restore. No Lua code from the index is executed.
func selectedSession(ctx context.Context, world *os.Root) (string, [32]byte, os.FileInfo, error) {
	var zero [32]byte
	save, err := openDirectory(world, "save")
	if err != nil {
		return "", zero, nil, err
	}
	defer save.Close()
	var session string
	var fingerprint [32]byte
	indexed := false
	for _, name := range []string{"shardindex", "saveindex"} {
		_, statErr := save.Lstat(name)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return "", zero, nil, failure("SAVE_INVALID")
		}
		data, _, readErr := readRegular(save, name)
		if readErr != nil {
			return "", zero, nil, readErr
		}
		root, parseErr := parse(ctx, data)
		if parseErr != nil {
			return "", zero, nil, parseErr
		}
		if name == "saveindex" {
			slots := child(root, "slots")
			if slots != nil {
				root, _ = (*slots)[float64(1)].(*table)
			} else {
				root = nil
			}
			if root == nil {
				return "", zero, nil, failure("SAVE_INVALID")
			}
		}
		if raw := field(root, "session_id"); raw != nil {
			var ok bool
			session, ok = raw.(string)
			if !ok || !sessionPattern.MatchString(session) {
				return "", zero, nil, failure("SAVE_INVALID")
			}
		}
		fingerprint = sha256.Sum256(append([]byte(name+"\x00"), data...))
		indexed = true
		break
	}
	sessions, err := openDirectory(save, "session")
	if err != nil {
		return "", zero, nil, err
	}
	defer sessions.Close()
	if !indexed {
		f, err := sessions.Open(".")
		if err != nil {
			return "", zero, nil, failure("SAVE_INVALID")
		}
		entries, err := f.ReadDir(4097)
		f.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return "", zero, nil, failure("SAVE_INVALID")
		}
		if len(entries) > 4096 {
			return "", zero, nil, failure("SAVE_LIMIT_EXCEEDED")
		}
		for _, entry := range entries {
			if !sessionPattern.MatchString(entry.Name()) {
				continue
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return "", zero, nil, failure("SAVE_INVALID")
			}
			if !entry.IsDir() {
				continue
			}
			if session != "" {
				return "", zero, nil, failure("SOURCE_AMBIGUOUS")
			}
			session = entry.Name()
		}
		fingerprint = sha256.Sum256([]byte("directory\x00" + session))
	}
	if session == "" {
		return "", zero, nil, failure("PLAYER_SNAPSHOT_NOT_FOUND")
	}
	info, err := sessions.Lstat(session)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", zero, nil, failure("SAVE_INVALID")
	}
	return session, fingerprint, info, nil
}
