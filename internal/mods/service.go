package mods

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/backups"
	"dont/internal/rooms"
	lua "github.com/yuin/gopher-lua"
)

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
	World(string, string) (rooms.World, error)
	Worlds(string) ([]rooms.World, error)
}

type Runtime interface {
	IsRunning(context.Context, string, string) (bool, error)
}

type BackupCreator interface {
	Create(context.Context, string, string, backups.Kind, string) (backups.Backup, error)
}

type Config struct {
	SaveRoot            string
	ServerRoot          string
	WorkshopContentRoot string
	UGCRoot             string
	AppID               string
}

type Service struct {
	config   Config
	rooms    RoomCatalog
	runtime  Runtime
	backups  BackupCreator
	metadata MetadataProvider
	parser   ModInfoParser
	runner   DownloadRunner
	now      func() time.Time
	locksMu  sync.Mutex
	locks    map[string]*sync.Mutex
}

func NewService(config Config, roomCatalog RoomCatalog, runtime Runtime, backupCreator BackupCreator, metadata MetadataProvider, parser ModInfoParser, runner DownloadRunner) (*Service, error) {
	if roomCatalog == nil || runtime == nil || backupCreator == nil || metadata == nil || parser == nil || runner == nil {
		return nil, errors.New("rooms, runtime, backups, metadata, parser, and runner are required")
	}
	if strings.TrimSpace(config.AppID) == "" {
		config.AppID = "322330"
	}
	var err error
	for _, item := range []struct {
		name string
		path *string
	}{
		{"save root", &config.SaveRoot}, {"server root", &config.ServerRoot},
		{"workshop content root", &config.WorkshopContentRoot}, {"UGC root", &config.UGCRoot},
	} {
		*item.path, err = absolutePath(*item.path)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", item.name, err)
		}
	}
	return &Service{
		config: config, rooms: roomCatalog, runtime: runtime, backups: backupCreator,
		metadata: metadata, parser: parser, runner: runner, now: time.Now, locks: make(map[string]*sync.Mutex),
	}, nil
}

func (s *Service) Search(ctx context.Context, query string, page, pageSize int) (SearchResult, error) {
	result, err := s.metadata.Search(ctx, query, page, pageSize)
	for index := range result.Items {
		normalizeSteamModCollections(&result.Items[index])
	}
	return result, err
}

func (s *Service) roomLock(roomID string) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	if s.locks[roomID] == nil {
		s.locks[roomID] = &sync.Mutex{}
	}
	return s.locks[roomID]
}

func (s *Service) List(ctx context.Context, roomID string) (ModList, error) {
	lock := s.roomLock(roomID)
	lock.Lock()
	defer lock.Unlock()
	room, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return ModList{}, err
	}
	worlds, err := s.rooms.Worlds(roomID)
	if err != nil {
		return ModList{}, err
	}
	type aggregate struct {
		configured []string
		enabled    []string
		installed  []string
		loaded     []string
		running    bool
	}
	aggregates := make(map[string]*aggregate)
	ensure := func(id string) *aggregate {
		if aggregates[id] == nil {
			aggregates[id] = &aggregate{}
		}
		return aggregates[id]
	}
	for _, world := range worlds {
		document, loadErr := loadModOverride(filepath.Join(roomPath, world.DirectoryName, "modoverrides.lua"))
		if loadErr != nil {
			return ModList{}, fmt.Errorf("read %s modoverrides.lua: %w", world.Name, loadErr)
		}
		for _, entry := range document.root.entries {
			if entry.key.kind.String() != "string" || !strings.HasPrefix(entry.key.text, "workshop-") {
				continue
			}
			id := strings.TrimPrefix(entry.key.text, "workshop-")
			if !validModID(id) {
				continue
			}
			item := ensure(id)
			item.configured = append(item.configured, world.ID)
			if modEnabled(entry.value) {
				item.enabled = append(item.enabled, world.ID)
			}
		}
		running, _ := s.runtime.IsRunning(ctx, roomID, world.ID)
		for _, id := range scanInstalledIDs(s.ugcCandidates(room, world)) {
			item := ensure(id)
			item.installed = append(item.installed, world.ID)
			if running && logContainsMod(filepath.Join(roomPath, world.DirectoryName, "server_log.txt"), id) {
				item.loaded = append(item.loaded, world.ID)
			}
		}
	}
	setup, err := loadSetup(s.setupPath())
	if err != nil {
		return ModList{}, err
	}
	for _, id := range setupIDs(setup.data) {
		ensure(id)
	}
	for _, id := range scanNumericDirectories(s.config.WorkshopContentRoot) {
		ensure(id)
	}
	manifest, manifestErr := loadWorkshopManifest(s.workshopManifestPath())
	for id := range manifest {
		ensure(id)
	}
	ids := make([]string, 0, len(aggregates))
	for id := range aggregates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	metadata, metadataErr := s.metadata.Details(ctx, ids)
	items := make([]ModState, 0, len(ids))
	for _, id := range ids {
		aggregate := aggregates[id]
		state := ModState{
			SteamMod: metadata[id], Configured: len(aggregate.configured) > 0, Enabled: len(aggregate.enabled) > 0,
			Installed: len(aggregate.installed) > 0, Loaded: len(aggregate.loaded) > 0,
			ConfiguredWorlds: uniqueStrings(aggregate.configured), EnabledWorlds: uniqueStrings(aggregate.enabled),
			InstalledWorlds: uniqueStrings(aggregate.installed), LoadedWorlds: uniqueStrings(aggregate.loaded), Warnings: []string{},
		}
		state.ID = id
		normalizeSteamModCollections(&state.SteamMod)
		if manifestItem, ok := manifest[id]; ok {
			state.WorkshopManifest = true
			if !manifestItem.UpdatedAt.IsZero() {
				updated := manifestItem.UpdatedAt
				state.ManifestUpdatedAt = &updated
			}
		}
		downloadPath := s.downloadedPath(id)
		state.Downloaded = directoryExists(downloadPath)
		modified := latestModTime(downloadPath)
		if state.ManifestUpdatedAt != nil && state.ManifestUpdatedAt.After(modified) {
			modified = *state.ManifestUpdatedAt
		}
		if !modified.IsZero() {
			modified = modified.UTC()
			state.LocalUpdatedAt = &modified
		}
		if state.WorkshopManifest && !state.Downloaded {
			state.Warnings = append(state.Warnings, "Steam 清单存在记录，但 Workshop 下载目录缺失")
		}
		modInfoPath := filepath.Join(downloadPath, "modinfo.lua")
		if state.Downloaded {
			if _, statErr := safeRegularFile(modInfoPath); statErr != nil {
				state.Warnings = append(state.Warnings, "已下载目录缺少安全可读的 modinfo.lua")
			} else {
				probeCtx, cancel := context.WithTimeout(ctx, embeddedParserTimeout+externalParserTimeout+time.Second)
				if parsed, parseErr := s.parser.Parse(probeCtx, id, modInfoPath); parseErr == nil {
					state.Parser = parsed.Parser
					state.FallbackUsed = parsed.FallbackUsed
					state.FallbackReason = parsed.FallbackReason
					state.Warnings = append(state.Warnings, parsed.Warnings...)
					if state.Name == "" {
						state.Name, _ = parsed.Values["name"].(string)
					}
				} else {
					state.Warnings = append(state.Warnings, "modinfo.lua 解析失败："+sanitizeParserError(parseErr))
				}
				cancel()
			}
		}
		state.applyHealth()
		items = append(items, state)
	}
	result := ModList{Items: items, Total: len(items), CheckedAt: s.now().UTC()}
	if metadataErr != nil {
		result.MetadataWarning = metadataErr.Error()
	}
	if manifestErr != nil {
		if result.MetadataWarning != "" {
			result.MetadataWarning += "; "
		}
		result.MetadataWarning += manifestErr.Error()
	}
	for _, item := range items {
		if item.Health == HealthHealthy || item.Health == HealthDisabled {
			result.Healthy++
		} else {
			result.Attention++
		}
	}
	return result, nil
}

func (state *ModState) applyHealth() {
	state.Health, state.HealthMessage, state.RepairAction = HealthHealthy, "Mod 文件与配置状态正常", ""
	if state.Downloaded && len(state.Warnings) > 0 && strings.Contains(state.Warnings[0], "缺少") {
		state.Health, state.HealthMessage, state.RepairAction = HealthCorrupt, "下载目录不完整", "repair"
		return
	}
	if state.Configured && !state.Downloaded && !state.Installed {
		state.Health, state.HealthMessage, state.RepairAction = HealthNotDownloaded, "已配置但尚未下载", "repair"
		return
	}
	if state.Configured && state.Downloaded && !state.Installed {
		state.Health, state.HealthMessage, state.RepairAction = HealthNotInstalled, "Workshop 已下载，UGC 尚未安装；启动分片后由 DST 完成安装", "restart"
		return
	}
	if state.LocalUpdatedAt != nil && !state.UpdatedAt.IsZero() && state.UpdatedAt.After(state.LocalUpdatedAt.Add(time.Second)) {
		state.Health, state.HealthMessage, state.RepairAction = HealthUpdateAvailable, "Steam Workshop 有更新", "update"
		return
	}
	if len(state.Warnings) > 0 {
		state.Health, state.HealthMessage, state.RepairAction = HealthParseWarning, "modinfo.lua 需要 Lua fallback", "configure"
		return
	}
	if state.Configured && !state.Enabled {
		state.Health, state.HealthMessage = HealthDisabled, "所有已配置分片均已禁用"
	}
}

func (s *Service) Install(ctx context.Context, jobID, roomID string, request InstallRequest, output io.Writer) (ActionResult, error) {
	if !validModID(request.ModID) {
		return ActionResult{}, &FieldError{Fields: map[string]string{"modId": "Workshop ID 必须为数字"}}
	}
	lock := s.roomLock(roomID)
	lock.Lock()
	defer lock.Unlock()
	room, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return ActionResult{}, err
	}
	worlds, err := s.selectWorlds(roomID, request.WorldIDs)
	if err != nil {
		return ActionResult{}, err
	}
	ids, err := s.resolveDependencies(ctx, request.ModID, request.IncludeDependencies)
	if err != nil {
		return ActionResult{}, err
	}
	targets := make([]directoryTarget, 0, len(ids))
	for _, id := range ids {
		targets = append(targets, directoryTarget{root: s.config.WorkshopContentRoot, path: s.downloadedPath(id)})
	}
	staged, err := stageDirectories(targets)
	if err != nil {
		return ActionResult{}, err
	}
	downloadErr := s.runner.Download(ctx, ids, false, output)
	if downloadErr == nil {
		downloadErr = s.verifyDownloads(ids)
	}
	if downloadErr != nil {
		return ActionResult{}, errors.Join(downloadErr, restoreDirectories(staged))
	}
	if err := discardDirectories(staged); err != nil {
		return ActionResult{}, fmt.Errorf("remove staged Mod cache: %w", err)
	}
	configurationChanged := false
	mutations, err := s.modMutations(roomPath, worlds, func(document *modOverrideDocument) error {
		for _, id := range ids {
			entry, ok := document.mod(id)
			_, hasConfiguration := modConfiguration(entry)
			if !ok || entry.kind != lua.LTTable || modEnabled(entry) != request.Enabled || !hasConfiguration {
				configurationChanged = true
			}
			ensureModEntry(document, id, request.Enabled)
		}
		return nil
	})
	if err != nil {
		return ActionResult{}, err
	}
	if !configurationChanged {
		mutations = nil
	}
	setupMutation, err := s.setupMutation(ids, nil)
	if err != nil {
		return ActionResult{}, err
	}
	mutations = changedMutations(append(mutations, setupMutation))
	result := ActionResult{ModIDs: ids, Message: "Mod 已下载；分片配置原本已是目标状态"}
	if len(mutations) == 0 {
		return result, nil
	}
	backup, err := s.protectionBackup(ctx, room, "Mod 安装", jobID)
	if err != nil {
		return ActionResult{}, err
	}
	if err := applyFileMutations(mutations); err != nil {
		return ActionResult{}, err
	}
	result.ProtectionBackupID = backup.ID
	result.Message = "Mod 已下载并写入分片配置"
	return result, nil
}

func (s *Service) Update(ctx context.Context, roomID, modID string, output io.Writer) (ActionResult, error) {
	if !validModID(modID) {
		return ActionResult{}, ErrInvalidModID
	}
	lock := s.roomLock(roomID)
	lock.Lock()
	defer lock.Unlock()
	if _, _, err := s.resolveRoom(roomID); err != nil {
		return ActionResult{}, err
	}
	staged, err := stageDirectories([]directoryTarget{{root: s.config.WorkshopContentRoot, path: s.downloadedPath(modID)}})
	if err != nil {
		return ActionResult{}, err
	}
	downloadErr := s.runner.Download(ctx, []string{modID}, true, output)
	if downloadErr == nil {
		downloadErr = s.verifyDownloads([]string{modID})
	}
	if downloadErr != nil {
		return ActionResult{}, errors.Join(downloadErr, restoreDirectories(staged))
	}
	if err := discardDirectories(staged); err != nil {
		return ActionResult{}, fmt.Errorf("remove staged Mod cache: %w", err)
	}
	return ActionResult{ModIDs: []string{modID}, Message: "Workshop 文件已更新并完成校验"}, nil
}

func (s *Service) Enable(ctx context.Context, jobID, roomID, modID string, request EnableRequest) (ActionResult, error) {
	if !validModID(modID) {
		return ActionResult{}, ErrInvalidModID
	}
	lock := s.roomLock(roomID)
	lock.Lock()
	defer lock.Unlock()
	room, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return ActionResult{}, err
	}
	worlds, err := s.selectWorlds(roomID, request.WorldIDs)
	if err != nil {
		return ActionResult{}, err
	}
	found := false
	changed := false
	mutations, err := s.modMutations(roomPath, worlds, func(document *modOverrideDocument) error {
		entry, ok := document.mod(modID)
		if !ok {
			return nil
		}
		found = true
		if modEnabled(entry) == request.Enabled {
			return nil
		}
		changed = true
		entry.setStringEntry("enabled", boolNode(request.Enabled))
		return nil
	})
	if err != nil {
		return ActionResult{}, err
	}
	if !found {
		return ActionResult{}, ErrModNotConfigured
	}
	if !changed {
		return ActionResult{}, ErrNoChanges
	}
	mutations = changedMutations(mutations)
	if len(mutations) == 0 {
		return ActionResult{}, ErrNoChanges
	}
	backup, err := s.protectionBackup(ctx, room, "Mod 启停", jobID)
	if err != nil {
		return ActionResult{}, err
	}
	if err := applyFileMutations(mutations); err != nil {
		return ActionResult{}, err
	}
	message := "Mod 已禁用"
	if request.Enabled {
		message = "Mod 已启用"
	}
	return ActionResult{ModIDs: []string{modID}, ProtectionBackupID: backup.ID, Message: message}, nil
}

func (s *Service) Uninstall(ctx context.Context, jobID, roomID, modID string, request ModActionRequest) (ActionResult, error) {
	if !validModID(modID) {
		return ActionResult{}, ErrInvalidModID
	}
	lock := s.roomLock(roomID)
	lock.Lock()
	defer lock.Unlock()
	room, roomPath, err := s.resolveRoom(roomID)
	if err != nil {
		return ActionResult{}, err
	}
	if request.Confirmation != room.Name {
		return ActionResult{}, ErrConfirmationNeeded
	}
	worlds, err := s.selectWorlds(roomID, request.WorldIDs)
	if err != nil {
		return ActionResult{}, err
	}
	mutations, err := s.modMutations(roomPath, worlds, func(document *modOverrideDocument) error {
		document.root.setStringEntry("workshop-"+modID, nil)
		return nil
	})
	if err != nil {
		return ActionResult{}, err
	}
	stillConfigured, err := s.configuredOutside(roomPath, roomID, worlds, modID)
	if err != nil {
		return ActionResult{}, err
	}
	if !stillConfigured {
		setupMutation, setupErr := s.setupMutation(nil, []string{modID})
		if setupErr != nil {
			return ActionResult{}, setupErr
		}
		mutations = append(mutations, setupMutation)
		stillConfigured = containsString(setupIDs(setupMutation.data), modID)
	}
	mutations = changedMutations(mutations)
	removeFiles := request.RemoveFiles && !stillConfigured
	filesChanged := false
	if removeFiles {
		filesChanged, err = s.modFilesExist(room, worlds, modID)
		if err != nil {
			return ActionResult{}, err
		}
	}
	if len(mutations) == 0 && !filesChanged {
		return ActionResult{}, ErrNoChanges
	}
	result := ActionResult{ModIDs: []string{modID}, Message: "Mod 已从所选分片移除"}
	if len(mutations) > 0 {
		backup, backupErr := s.protectionBackup(ctx, room, "Mod 卸载", jobID)
		if backupErr != nil {
			return ActionResult{}, backupErr
		}
		if err := applyFileMutations(mutations); err != nil {
			return ActionResult{}, err
		}
		result.ProtectionBackupID = backup.ID
	}
	if filesChanged {
		if err := s.removeModFiles(room, worlds, modID); err != nil {
			return ActionResult{}, fmt.Errorf("configuration was removed but files could not be deleted: %w", err)
		}
	}
	return result, nil
}

func (s *Service) Repair(ctx context.Context, roomID, modID string, request ModActionRequest, output io.Writer) (ActionResult, error) {
	if !validModID(modID) {
		return ActionResult{}, ErrInvalidModID
	}
	lock := s.roomLock(roomID)
	lock.Lock()
	defer lock.Unlock()
	room, _, err := s.resolveRoom(roomID)
	if err != nil {
		return ActionResult{}, err
	}
	if request.Confirmation != room.Name {
		return ActionResult{}, ErrConfirmationNeeded
	}
	worlds, err := s.selectWorlds(roomID, request.WorldIDs)
	if err != nil {
		return ActionResult{}, err
	}
	staged, err := stageDirectories(s.modFileTargets(room, worlds, modID))
	if err != nil {
		return ActionResult{}, err
	}
	downloadErr := s.runner.Download(ctx, []string{modID}, true, output)
	if downloadErr == nil {
		downloadErr = s.verifyDownloads([]string{modID})
	}
	if downloadErr != nil {
		return ActionResult{}, errors.Join(downloadErr, restoreDirectories(staged))
	}
	if err := discardDirectories(staged); err != nil {
		return ActionResult{}, fmt.Errorf("remove staged Mod cache: %w", err)
	}
	return ActionResult{ModIDs: []string{modID}, Message: "损坏缓存已清理并重新下载；运行中的分片需要重启后重新安装 UGC"}, nil
}

func (s *Service) CheckUpdates(ctx context.Context, roomID string) (ActionResult, error) {
	list, err := s.List(ctx, roomID)
	if err != nil {
		return ActionResult{}, err
	}
	updates := make([]string, 0)
	for _, item := range list.Items {
		if item.Health == HealthUpdateAvailable {
			updates = append(updates, item.ID)
		}
	}
	return ActionResult{ModIDs: updates, Message: fmt.Sprintf("更新检查完成：%d 个 Mod 可更新", len(updates))}, nil
}

func (s *Service) resolveDependencies(ctx context.Context, root string, include bool) ([]string, error) {
	result := []string{root}
	if !include {
		return result, nil
	}
	seen := map[string]bool{root: true}
	for index := 0; index < len(result); index++ {
		if len(result) > 100 {
			return nil, errors.New("Mod dependency graph exceeds 100 items")
		}
		details, err := s.metadata.Details(ctx, []string{result[index]})
		if err != nil {
			return nil, err
		}
		item, ok := details[result[index]]
		if !ok {
			return nil, fmt.Errorf("Steam Workshop item %s was not found", result[index])
		}
		for _, dependency := range item.Dependencies {
			if !seen[dependency] {
				seen[dependency] = true
				result = append(result, dependency)
			}
		}
	}
	return result, nil
}

func (s *Service) verifyDownloads(ids []string) error {
	for _, id := range ids {
		if !directoryExists(s.downloadedPath(id)) {
			return fmt.Errorf("SteamCMD completed but Workshop item %s is missing", id)
		}
		if _, err := safeRegularFile(filepath.Join(s.downloadedPath(id), "modinfo.lua")); err != nil {
			return fmt.Errorf("SteamCMD completed but Workshop item %s has no safe modinfo.lua: %w", id, err)
		}
	}
	return nil
}

func (s *Service) modMutations(roomPath string, worlds []rooms.World, update func(*modOverrideDocument) error) ([]fileMutation, error) {
	mutations := make([]fileMutation, 0, len(worlds))
	for _, world := range worlds {
		path := filepath.Join(roomPath, world.DirectoryName, "modoverrides.lua")
		document, err := loadModOverride(path)
		if err != nil {
			return nil, err
		}
		if err := update(&document); err != nil {
			return nil, err
		}
		next, err := renderModOverride(document)
		if err != nil {
			return nil, err
		}
		mutations = append(mutations, fileMutation{path: path, data: next, previous: fileSnapshot{data: document.data, mode: document.mode, exists: document.exists}})
	}
	return mutations, nil
}

func (s *Service) setupMutation(add, remove []string) (fileMutation, error) {
	path := s.setupPath()
	previous, err := loadSetup(path)
	if err != nil {
		return fileMutation{}, err
	}
	managed := managedSetupIDs(previous.data)
	existing := make(map[string]bool)
	for _, id := range setupIDs(previous.data) {
		existing[id] = true
	}
	removeSet := make(map[string]bool, len(remove))
	for _, id := range remove {
		removeSet[id] = true
	}
	nextIDs := make([]string, 0, len(managed)+len(add))
	for _, id := range managed {
		if !removeSet[id] {
			nextIDs = append(nextIDs, id)
		}
	}
	for _, id := range add {
		if !existing[id] && !removeSet[id] {
			nextIDs = append(nextIDs, id)
		}
	}
	next, err := renderManagedSetup(previous.data, nextIDs)
	return fileMutation{path: path, data: next, previous: previous}, err
}

func managedSetupIDs(data []byte) []string {
	source := string(data)
	begin, end := strings.Index(source, managedSetupBegin), strings.Index(source, managedSetupEnd)
	if begin < 0 || end < begin {
		return []string{}
	}
	return setupIDs([]byte(source[begin:end]))
}

func changedMutations(values []fileMutation) []fileMutation {
	result := make([]fileMutation, 0, len(values))
	for _, value := range values {
		if value.previous.exists && string(value.previous.data) == string(value.data) {
			continue
		}
		result = append(result, value)
	}
	return result
}

func (s *Service) configuredOutside(roomPath, roomID string, selected []rooms.World, modID string) (bool, error) {
	selectedIDs := make(map[string]bool, len(selected))
	for _, world := range selected {
		selectedIDs[world.ID] = true
	}
	worlds, err := s.rooms.Worlds(roomID)
	if err != nil {
		return false, err
	}
	for _, world := range worlds {
		if selectedIDs[world.ID] {
			continue
		}
		document, err := loadModOverride(filepath.Join(roomPath, world.DirectoryName, "modoverrides.lua"))
		if err != nil {
			return false, err
		}
		if _, ok := document.mod(modID); ok {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) removeModFiles(room rooms.Room, worlds []rooms.World, modID string) error {
	var result error
	for _, target := range s.modFileTargets(room, worlds, modID) {
		result = errors.Join(result, safeRemoveDirectory(target.root, target.path))
	}
	return result
}

func (s *Service) modFileTargets(room rooms.Room, worlds []rooms.World, modID string) []directoryTarget {
	downloadPath := s.downloadedPath(modID)
	targets := []directoryTarget{{root: s.config.WorkshopContentRoot, path: downloadPath}}
	seen := map[string]bool{downloadPath: true}
	for _, world := range worlds {
		for _, candidate := range s.ugcCandidates(room, world) {
			path := filepath.Join(candidate, modID)
			if !seen[path] {
				targets = append(targets, directoryTarget{root: s.config.UGCRoot, path: path})
				seen[path] = true
			}
		}
	}
	return targets
}

func (s *Service) modFilesExist(room rooms.Room, worlds []rooms.World, modID string) (bool, error) {
	for _, target := range s.modFileTargets(room, worlds, modID) {
		path, err := safeDirectoryTarget(target.root, target.path)
		if err != nil {
			return false, err
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, errors.New("refusing to inspect an unsafe Mod path")
		}
		return true, nil
	}
	return false, nil
}

func (s *Service) protectionBackup(ctx context.Context, room rooms.Room, scope, jobID string) (backups.Backup, error) {
	value, err := s.backups.Create(ctx, room.ID, scope+"前保护备份 "+s.now().Format("2006-01-02 15:04:05"), backups.KindProtection, jobID)
	if err != nil {
		return backups.Backup{}, fmt.Errorf("create protection backup: %w", err)
	}
	return value, nil
}

func (s *Service) resolveRoom(roomID string) (rooms.Room, string, error) {
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return rooms.Room{}, "", err
	}
	if !room.Managed {
		return rooms.Room{}, "", ErrRoomNotManaged
	}
	path, err := safeDirectory(s.config.SaveRoot, room.DirectoryName)
	return room, path, err
}

func (s *Service) selectWorlds(roomID string, worldIDs []string) ([]rooms.World, error) {
	worlds, err := s.rooms.Worlds(roomID)
	if err != nil {
		return nil, err
	}
	if len(worldIDs) == 0 {
		return worlds, nil
	}
	wanted := make(map[string]bool, len(worldIDs))
	for _, id := range worldIDs {
		wanted[id] = true
	}
	result := make([]rooms.World, 0, len(wanted))
	for _, world := range worlds {
		if wanted[world.ID] {
			result = append(result, world)
			delete(wanted, world.ID)
		}
	}
	if len(wanted) > 0 {
		return nil, &FieldError{Fields: map[string]string{"worldIds": "包含不属于当前房间的世界"}}
	}
	return result, nil
}

func (s *Service) setupPath() string {
	return filepath.Join(s.config.ServerRoot, "mods", "dedicated_server_mods_setup.lua")
}

func (s *Service) downloadedPath(modID string) string {
	return filepath.Join(s.config.WorkshopContentRoot, modID)
}

func (s *Service) workshopManifestPath() string {
	workshopRoot := filepath.Dir(filepath.Dir(s.config.WorkshopContentRoot))
	return filepath.Join(workshopRoot, "appworkshop_"+s.config.AppID+".acf")
}

func (s *Service) ugcCandidates(room rooms.Room, world rooms.World) []string {
	return []string{
		filepath.Join(s.config.UGCRoot, room.DirectoryName, world.DirectoryName, "content", s.config.AppID),
		filepath.Join(s.config.UGCRoot, room.DirectoryName, "content", s.config.AppID),
		filepath.Join(s.config.UGCRoot, "content", s.config.AppID),
	}
}

func scanInstalledIDs(roots []string) []string {
	values := make([]string, 0)
	for _, root := range roots {
		values = append(values, scanNumericDirectories(root)...)
	}
	return uniqueModIDs(values)
}

func scanNumericDirectories(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return []string{}
	}
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && entry.Type()&os.ModeSymlink == 0 && validModID(entry.Name()) {
			result = append(result, entry.Name())
		}
	}
	return result
}

func logContainsMod(path, modID string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	const tailBytes int64 = 2 * 1024 * 1024
	if info.Size() > tailBytes {
		_, _ = file.Seek(info.Size()-tailBytes, io.SeekStart)
	}
	data, _ := io.ReadAll(io.LimitReader(file, tailBytes))
	text := strings.ToLower(string(data))
	return strings.Contains(text, "workshop-"+modID) && (strings.Contains(text, "loading mod") || strings.Contains(text, "modindex"))
}

func latestModTime(root string) time.Time {
	var latest time.Time
	_ = filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil || entry.Type()&os.ModeSymlink != 0 {
			return filepath.SkipDir
		}
		info, infoErr := entry.Info()
		if infoErr == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
		return nil
	})
	return latest
}

func directoryExists(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func normalizeSteamModCollections(value *SteamMod) {
	if value.Dependencies == nil {
		value.Dependencies = []string{}
	}
	if value.Tags == nil {
		value.Tags = []string{}
	}
}

func absolutePath(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("path is required")
	}
	return filepath.Abs(value)
}

func safeDirectory(root, name string) (string, error) {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00\r\n") {
		return "", errors.New("unsafe room or world directory")
	}
	path := filepath.Join(root, name)
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", errors.New("path escapes its root")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("directory is unsafe")
	}
	return path, nil
}
