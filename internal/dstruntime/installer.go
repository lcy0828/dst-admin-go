package dstruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/rooms"

	"github.com/yuin/gopher-lua/parse"
)

const (
	customCommandsName = "customcommands.lua"
	managedDirectory   = "dst-admin"
	manifestName       = "manifest.json"
	maxCustomBytes     = int64(2 * 1024 * 1024)
	maxManagedBytes    = int64(2 * 1024 * 1024)
)

const managedLoader = `-- DST-ADMIN MANAGED BLOCK BEGIN protocol=2 version=2.4.0
do
    TheSim:GetPersistentString("../dst-admin/bootstrap.lua", function(success, source)
        if not success or type(source) ~= "string" then
            print("[DST-ADMIN-RUNTIME ERROR] code=BOOTSTRAP_UNAVAILABLE")
            return
        end
        local chunk, compile_error = loadstring(source)
        if chunk == nil then
            print("[DST-ADMIN-RUNTIME ERROR] code=BOOTSTRAP_COMPILE_FAILED message=" .. tostring(compile_error))
            return
        end
        local executed, runtime_error = xpcall(chunk, debug.traceback)
        if not executed then
            print("[DST-ADMIN-RUNTIME ERROR] code=BOOTSTRAP_EXECUTE_FAILED message=" .. tostring(runtime_error))
        end
    end)
end
-- DST-ADMIN MANAGED BLOCK END`

var (
	beginMarker = regexp.MustCompile(`(?m)^-- DST-ADMIN MANAGED BLOCK BEGIN[^\r\n]*$`)
	endMarker   = regexp.MustCompile(`(?m)^-- DST-ADMIN MANAGED BLOCK END[ \t]*$`)
)

type Catalog interface {
	Room(string) (rooms.Room, error)
	World(string, string) (rooms.World, error)
	Worlds(string) ([]rooms.World, error)
}

type manifest struct {
	Version         string            `json:"version"`
	ProtocolVersion int               `json:"protocolVersion"`
	Assets          map[string]string `json:"assets"`
	InstalledAt     time.Time         `json:"installedAt"`
}

type Manager struct {
	root    string
	rooms   Catalog
	now     func() time.Time
	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

func NewManager(saveRoot string, roomCatalog Catalog) (*Manager, error) {
	if roomCatalog == nil || strings.TrimSpace(saveRoot) == "" {
		return nil, errors.New("save root and room catalog are required")
	}
	root, err := filepath.Abs(strings.TrimSpace(saveRoot))
	if err != nil {
		return nil, fmt.Errorf("resolve save root: %w", err)
	}
	return &Manager{root: filepath.Clean(root), rooms: roomCatalog, now: time.Now, locks: make(map[string]*sync.Mutex)}, nil
}

func (m *Manager) InstallRoom(ctx context.Context, roomID string) ([]WorldStatus, error) {
	room, err := m.rooms.Room(roomID)
	if err != nil {
		return nil, err
	}
	if !room.Managed {
		return nil, rooms.ErrRoomNotManaged
	}
	worlds, err := m.rooms.Worlds(room.ID)
	if err != nil {
		return nil, err
	}
	result := make([]WorldStatus, 0, len(worlds))
	for _, world := range worlds {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		status, installErr := m.install(room, world)
		result = append(result, status)
		if installErr != nil {
			return result, installErr
		}
	}
	return result, nil
}

func (m *Manager) InstallWorld(ctx context.Context, roomID, worldID string) (WorldStatus, error) {
	if err := ctx.Err(); err != nil {
		return WorldStatus{}, err
	}
	room, err := m.rooms.Room(roomID)
	if err != nil {
		return WorldStatus{}, err
	}
	if !room.Managed {
		return WorldStatus{}, rooms.ErrRoomNotManaged
	}
	world, err := m.rooms.World(room.ID, worldID)
	if err != nil {
		return WorldStatus{}, err
	}
	return m.install(room, world)
}

func (m *Manager) Prepare(_ context.Context, roomName, worldName string) error {
	room, err := m.rooms.Room(rooms.EncodeID(roomName))
	if err != nil {
		return err
	}
	world, err := m.rooms.World(room.ID, rooms.EncodeID(worldName))
	if err != nil {
		return err
	}
	_, err = m.install(room, world)
	return err
}

func (m *Manager) StatusRoom(roomID string) ([]WorldStatus, error) {
	room, err := m.rooms.Room(roomID)
	if err != nil {
		return nil, err
	}
	worlds, err := m.rooms.Worlds(room.ID)
	if err != nil {
		return nil, err
	}
	result := make([]WorldStatus, 0, len(worlds))
	for _, world := range worlds {
		result = append(result, m.inspect(room, world))
	}
	return result, nil
}

func (m *Manager) Backups(roomID, worldID string) ([]Backup, error) {
	room, err := m.rooms.Room(roomID)
	if err != nil {
		return nil, err
	}
	world, err := m.rooms.World(room.ID, worldID)
	if err != nil {
		return nil, err
	}
	worldPath, err := m.worldPath(room, world)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(worldPath, backupDirectory))
	if os.IsNotExist(err) {
		return []Backup{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]Backup, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !backupIDPattern.MatchString(entry.Name()) {
			continue
		}
		manifest, _, readErr := readStateBackup(worldPath, entry.Name())
		if readErr != nil {
			continue
		}
		result = append(result, Backup{ID: manifest.ID, WorldID: world.ID, CreatedAt: manifest.CreatedAt.UTC(), Reason: manifest.Reason})
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result, nil
}

func (m *Manager) RollbackWorld(ctx context.Context, roomID, worldID, backupID string) (WorldStatus, error) {
	if err := ctx.Err(); err != nil {
		return WorldStatus{}, err
	}
	room, err := m.rooms.Room(roomID)
	if err != nil {
		return WorldStatus{}, err
	}
	if !room.Managed {
		return WorldStatus{}, rooms.ErrRoomNotManaged
	}
	world, err := m.rooms.World(room.ID, worldID)
	if err != nil {
		return WorldStatus{}, err
	}
	worldPath, err := m.worldPath(room, world)
	if err != nil {
		return WorldStatus{}, err
	}
	lock := m.lock(worldPath)
	lock.Lock()
	defer lock.Unlock()
	protectionBackup, err := m.createStateBackup(worldPath, "before-rollback")
	if err != nil {
		return WorldStatus{}, err
	}
	if err := m.restoreStateBackup(worldPath, backupID); err != nil {
		return WorldStatus{}, err
	}
	status := m.inspectAt(room, world, worldPath)
	status.Changed = true
	status.BackupPath = filepath.Join(backupDirectory, protectionBackup)
	status.Message = "已回滚 DST Admin 运行时；操作前状态已创建保护备份"
	return status, nil
}

func (m *Manager) UninstallWorld(ctx context.Context, roomID, worldID string) (WorldStatus, error) {
	if err := ctx.Err(); err != nil {
		return WorldStatus{}, err
	}
	room, err := m.rooms.Room(roomID)
	if err != nil {
		return WorldStatus{}, err
	}
	world, err := m.rooms.World(room.ID, worldID)
	if err != nil {
		return WorldStatus{}, err
	}
	worldPath, err := m.worldPath(room, world)
	if err != nil {
		return WorldStatus{}, err
	}
	lock := m.lock(worldPath)
	lock.Lock()
	defer lock.Unlock()
	customPath := filepath.Join(worldPath, customCommandsName)
	custom, mode, exists, err := readRegular(customPath, maxCustomBytes)
	if err != nil {
		return WorldStatus{}, err
	}
	if !exists {
		return WorldStatus{RoomID: room.ID, WorldID: world.ID, WorldName: world.Name, State: InstallStateMissing}, nil
	}
	updated, found, err := removeManagedBlock(custom)
	if err != nil {
		return WorldStatus{}, err
	}
	if !found {
		return WorldStatus{RoomID: room.ID, WorldID: world.ID, WorldName: world.Name, State: InstallStateMissing}, nil
	}
	backup, err := m.createStateBackup(worldPath, "before-uninstall")
	if err != nil {
		return WorldStatus{}, err
	}
	if len(bytes.TrimSpace(updated)) == 0 {
		if err := os.Remove(customPath); err != nil && !os.IsNotExist(err) {
			return WorldStatus{}, err
		}
	} else if err := atomicWrite(customPath, updated, mode); err != nil {
		return WorldStatus{}, err
	}
	if err := os.RemoveAll(filepath.Join(worldPath, managedDirectory)); err != nil {
		return WorldStatus{}, fmt.Errorf("remove managed runtime: %w", err)
	}
	return WorldStatus{RoomID: room.ID, WorldID: world.ID, WorldName: world.Name, State: InstallStateMissing, Changed: true, BackupPath: filepath.Join(backupDirectory, backup), Message: "DST Admin 运行时已卸载，用户脚本已保留"}, nil
}

func (m *Manager) install(room rooms.Room, world rooms.World) (WorldStatus, error) {
	worldPath, err := m.worldPath(room, world)
	if err != nil {
		return WorldStatus{}, err
	}
	lock := m.lock(worldPath)
	lock.Lock()
	defer lock.Unlock()
	status := WorldStatus{RoomID: room.ID, WorldID: world.ID, WorldName: world.Name, State: InstallStateMissing}
	customPath := filepath.Join(worldPath, customCommandsName)
	custom, mode, _, err := readRegular(customPath, maxCustomBytes)
	if err != nil {
		return status, err
	}
	updated, changed, err := upsertManagedBlock(custom)
	if err != nil {
		return status, err
	}
	assets, hashes, err := desiredAssets()
	if err != nil {
		return status, err
	}
	currentManifest, manifestExists, manifestErr := readManifest(filepath.Join(worldPath, managedDirectory, manifestName))
	if manifestErr != nil && !errors.Is(manifestErr, os.ErrNotExist) {
		return status, manifestErr
	}
	if manifestExists {
		if err := verifyManagedAssets(filepath.Join(worldPath, managedDirectory), currentManifest); err != nil {
			return WorldStatus{RoomID: room.ID, WorldID: world.ID, WorldName: world.Name, State: InstallStateInvalid, Message: err.Error()}, err
		}
	}
	assetsChanged := !manifestExists || currentManifest.Version != RuntimeVersion || currentManifest.ProtocolVersion != ProtocolVersion || !equalHashes(currentManifest.Assets, hashes)
	if err := ensureRuntimeOutputDirectory(worldPath); err != nil {
		return status, err
	}
	if !changed && !assetsChanged {
		status.State, status.Version, status.Protocol = InstallStateInstalled, RuntimeVersion, ProtocolVersion
		status.Message = "DST Admin 运行时已是最新版本"
		return status, nil
	}
	backup, err := m.createStateBackup(worldPath, "before-install")
	if err != nil {
		return status, err
	}
	previous, err := captureFiles(worldPath, append([]string{customCommandsName}, managedPaths()...))
	if err != nil {
		return status, err
	}
	rollback := func(cause error) error {
		return errors.Join(cause, restoreFiles(worldPath, previous))
	}
	managedRoot := filepath.Join(worldPath, managedDirectory)
	if err := os.MkdirAll(managedRoot, 0750); err != nil {
		return status, fmt.Errorf("create managed runtime directory: %w", err)
	}
	for _, name := range managedAssetNames {
		if err := validateLua(name, assets[name]); err != nil {
			return status, rollback(err)
		}
		if err := atomicWrite(filepath.Join(managedRoot, name), assets[name], 0640); err != nil {
			return status, rollback(err)
		}
	}
	nextManifest := manifest{Version: RuntimeVersion, ProtocolVersion: ProtocolVersion, Assets: hashes, InstalledAt: m.now().UTC()}
	encoded, err := json.MarshalIndent(nextManifest, "", "  ")
	if err != nil {
		return status, rollback(err)
	}
	encoded = append(encoded, '\n')
	if err := atomicWrite(filepath.Join(managedRoot, manifestName), encoded, 0640); err != nil {
		return status, rollback(err)
	}
	if mode == 0 {
		mode = 0640
	}
	if err := validateLua(customCommandsName, updated); err != nil {
		return status, rollback(err)
	}
	if err := atomicWrite(customPath, updated, mode); err != nil {
		return status, rollback(err)
	}
	return WorldStatus{RoomID: room.ID, WorldID: world.ID, WorldName: world.Name, State: InstallStateInstalled, Version: RuntimeVersion, Protocol: ProtocolVersion, Changed: true, BackupPath: filepath.Join(backupDirectory, backup), Message: "DST Admin 运行时已安装；运行中的分片将在下次重启后加载"}, nil
}

func (m *Manager) inspect(room rooms.Room, world rooms.World) WorldStatus {
	worldPath, err := m.worldPath(room, world)
	if err != nil {
		return WorldStatus{RoomID: room.ID, WorldID: world.ID, WorldName: world.Name, State: InstallStateInvalid, Message: err.Error()}
	}
	return m.inspectAt(room, world, worldPath)
}

func (m *Manager) inspectAt(room rooms.Room, world rooms.World, worldPath string) WorldStatus {
	status := WorldStatus{RoomID: room.ID, WorldID: world.ID, WorldName: world.Name, State: InstallStateMissing}
	custom, _, exists, err := readRegular(filepath.Join(worldPath, customCommandsName), maxCustomBytes)
	if err != nil {
		status.State, status.Message = InstallStateInvalid, err.Error()
		return status
	}
	if !exists {
		return status
	}
	_, found, err := locateManagedBlock(custom)
	if err != nil {
		status.State, status.Message = InstallStateInvalid, err.Error()
		return status
	}
	if !found {
		return status
	}
	current, exists, err := readManifest(filepath.Join(worldPath, managedDirectory, manifestName))
	if err != nil || !exists {
		status.State, status.Message = InstallStateInvalid, "运行时清单缺失或损坏"
		return status
	}
	status.Version, status.Protocol = current.Version, current.ProtocolVersion
	if err := verifyManagedAssets(filepath.Join(worldPath, managedDirectory), current); err != nil {
		status.State, status.Message = InstallStateInvalid, err.Error()
		return status
	}
	desired, _, err := desiredAssets()
	_ = desired
	if err != nil {
		status.State, status.Message = InstallStateInvalid, err.Error()
		return status
	}
	_, hashes, _ := desiredAssets()
	if current.Version != RuntimeVersion || current.ProtocolVersion != ProtocolVersion || !equalHashes(current.Assets, hashes) || !bytes.Contains(custom, []byte(managedLoader)) {
		status.State, status.Message = InstallStateOutdated, "运行时需要升级"
		return status
	}
	status.State, status.Message = InstallStateInstalled, "DST Admin 运行时已安装"
	return status
}

func (m *Manager) worldPath(room rooms.Room, world rooms.World) (string, error) {
	path := filepath.Join(m.root, room.DirectoryName, world.DirectoryName)
	relative, err := filepath.Rel(m.root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.IsAbs(relative) {
		return "", ErrUnsafeRuntimePath
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", ErrUnsafeRuntimePath
	}
	return path, nil
}

func ensureRuntimeOutputDirectory(worldPath string) error {
	current := worldPath
	for _, name := range []string{"save", "mod_config_data", managedDirectory} {
		current = filepath.Join(current, name)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err := os.Mkdir(current, 0750); err != nil && !os.IsExist(err) {
				return fmt.Errorf("create runtime output directory: %w", err)
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return fmt.Errorf("inspect runtime output directory: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: runtime output path %s is not a regular directory", ErrUnsafeRuntimePath, current)
		}
	}
	return nil
}

func (m *Manager) lock(path string) *sync.Mutex {
	m.locksMu.Lock()
	defer m.locksMu.Unlock()
	if m.locks[path] == nil {
		m.locks[path] = &sync.Mutex{}
	}
	return m.locks[path]
}

type blockLocation struct{ start, end int }

func locateManagedBlock(data []byte) (blockLocation, bool, error) {
	begins, ends := beginMarker.FindAllIndex(data, -1), endMarker.FindAllIndex(data, -1)
	if len(begins) == 0 && len(ends) == 0 {
		return blockLocation{}, false, nil
	}
	if len(begins) != 1 || len(ends) != 1 || begins[0][0] >= ends[0][0] {
		return blockLocation{}, false, ErrManagedBlockInvalid
	}
	end := ends[0][1]
	if end < len(data) && data[end] == '\r' {
		end++
	}
	if end < len(data) && data[end] == '\n' {
		end++
	}
	return blockLocation{start: begins[0][0], end: end}, true, nil
}

func upsertManagedBlock(data []byte) ([]byte, bool, error) {
	location, found, err := locateManagedBlock(data)
	if err != nil {
		return nil, false, err
	}
	loader := []byte(managedLoader + "\n")
	if found {
		if bytes.Equal(data[location.start:location.end], loader) {
			return append([]byte(nil), data...), false, nil
		}
		result := append([]byte(nil), data[:location.start]...)
		result = append(result, loader...)
		result = append(result, data[location.end:]...)
		return result, true, nil
	}
	result := append([]byte(nil), data...)
	if len(result) > 0 && result[len(result)-1] != '\n' {
		result = append(result, '\n')
	}
	if len(bytes.TrimSpace(result)) > 0 {
		result = append(result, '\n')
	}
	result = append(result, loader...)
	return result, true, nil
}

func removeManagedBlock(data []byte) ([]byte, bool, error) {
	location, found, err := locateManagedBlock(data)
	if err != nil || !found {
		return append([]byte(nil), data...), found, err
	}
	result := append([]byte(nil), data[:location.start]...)
	result = append(result, data[location.end:]...)
	return bytes.TrimRight(result, "\r\n\t "), true, nil
}

func desiredAssets() (map[string][]byte, map[string]string, error) {
	assets, hashes := make(map[string][]byte, len(managedAssetNames)), make(map[string]string, len(managedAssetNames))
	for _, name := range managedAssetNames {
		data, err := assetData(name)
		if err != nil {
			return nil, nil, err
		}
		assets[name] = data
		digest := sha256.Sum256(data)
		hashes[name] = hex.EncodeToString(digest[:])
	}
	return assets, hashes, nil
}

func verifyManagedAssets(root string, current manifest) error {
	for name, expected := range current.Assets {
		if !contains(managedAssetNames, name) {
			return fmt.Errorf("%w: unknown asset %s", ErrManagedFileChanged, name)
		}
		data, _, exists, err := readRegular(filepath.Join(root, name), maxManagedBytes)
		if err != nil || !exists {
			return fmt.Errorf("%w: %s", ErrManagedFileChanged, name)
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != expected {
			return fmt.Errorf("%w: %s", ErrManagedFileChanged, name)
		}
	}
	return nil
}

func readManifest(path string) (manifest, bool, error) {
	data, _, exists, err := readRegular(path, 64*1024)
	if err != nil || !exists {
		return manifest{}, exists, err
	}
	var value manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&value); err != nil {
		return manifest{}, true, fmt.Errorf("decode runtime manifest: %w", err)
	}
	if value.Version == "" || value.ProtocolVersion <= 0 || len(value.Assets) == 0 {
		return manifest{}, true, errors.New("runtime manifest is incomplete")
	}
	return value, true, nil
}

func equalHashes(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for name, value := range left {
		if right[name] != value {
			return false
		}
	}
	return true
}

func readRegular(path string, limit int64) ([]byte, os.FileMode, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > limit {
		return nil, 0, true, ErrUnsafeRuntimePath
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, true, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, 0, true, ErrUnsafeRuntimePath
	}
	return data, info.Mode().Perm(), true, nil
}

func validateLua(name string, data []byte) error {
	if _, err := parse.Parse(bytes.NewReader(data), name); err != nil {
		return fmt.Errorf("validate Lua syntax for %s: %w", name, err)
	}
	return nil
}

func validateCapturedLua(files []capturedFile) error {
	for _, file := range files {
		if !file.exists || !strings.HasSuffix(file.path, ".lua") {
			continue
		}
		if err := validateLua(file.path, file.data); err != nil {
			return err
		}
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if mode == 0 {
		mode = 0640
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".dst-admin-write-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return err
	}
	if directoryHandle, err := os.Open(directory); err == nil {
		_ = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	return nil
}

type capturedFile struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

func managedPaths() []string {
	paths := []string{filepath.Join(managedDirectory, manifestName)}
	for _, name := range managedAssetNames {
		paths = append(paths, filepath.Join(managedDirectory, name))
	}
	sort.Strings(paths)
	return paths
}

func captureFiles(root string, names []string) ([]capturedFile, error) {
	result := make([]capturedFile, 0, len(names))
	for _, name := range names {
		data, mode, exists, err := readRegular(filepath.Join(root, name), maxManagedBytes)
		if err != nil {
			return nil, err
		}
		result = append(result, capturedFile{path: name, data: data, mode: mode, exists: exists})
	}
	return result, nil
}

func restoreFiles(root string, files []capturedFile) error {
	var result error
	for _, file := range files {
		path := filepath.Join(root, file.path)
		if file.exists {
			result = errors.Join(result, atomicWrite(path, file.data, file.mode))
		} else if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
