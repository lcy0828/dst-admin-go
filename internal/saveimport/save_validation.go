package saveimport

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dont/internal/saveidentity"
)

// A configuration template may have no saved world. Once session data is
// present, importing it requires an index that actually selects a snapshot.
// Otherwise DST silently generates a new world instead of loading the save.
func inspectSavedWorld(worldRoot string) (int, error) {
	session, indexed, err := saveidentity.IndexedSession(worldRoot)
	if err != nil {
		return 0, fmt.Errorf("无法读取游戏存档索引: %w", err)
	}
	entries, err := os.ReadDir(filepath.Join(worldRoot, "save", "session"))
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	count, hasData, selectedSnapshot := 0, false, false
	for _, entry := range entries {
		if !entry.IsDir() {
			hasData = true
			continue
		}
		count++
		files, err := os.ReadDir(filepath.Join(worldRoot, "save", "session", entry.Name()))
		if err != nil {
			return count, err
		}
		for _, file := range files {
			hasData = true
			if entry.Name() != session || !file.Type().IsRegular() || strings.Trim(file.Name(), "0123456789") != "" {
				continue
			}
			info, err := file.Info()
			if err != nil {
				return count, err
			}
			selectedSnapshot = selectedSnapshot || info.Size() > 0
		}
	}
	if session != "" && !selectedSnapshot {
		return count, fmt.Errorf("索引选中的 session %q 缺少世界快照，请重新导出完整存档", session)
	}
	if hasData && (!indexed || session == "") {
		return count, fmt.Errorf("已有 session 数据，但缺少选中世界的有效 shardindex 或旧版 saveindex；直接启动可能生成新世界，请重新导出完整存档")
	}
	return count, nil
}
