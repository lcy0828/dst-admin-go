package players

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dont/internal/configuration"
	"dont/internal/dstruntime"
	"dont/internal/rooms"
	"dont/internal/shards"

	"github.com/google/uuid"
)

var playerIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type RoomCatalog interface {
	Room(string) (rooms.Room, error)
	World(string, string) (rooms.World, error)
	Worlds(string) ([]rooms.World, error)
}

type Runtime interface {
	IsRunning(context.Context, string, string) (bool, error)
}

type identifiedRuntime interface {
	StatusFor(context.Context, string, string) (shards.RuntimeStatus, error)
}

type Sender interface {
	Send(context.Context, string, string, string) error
}

type RuntimeCommander interface {
	ExecuteCommand(context.Context, string, string, dstruntime.CommandRequest) (dstruntime.CommandReceipt, error)
}

type AccessManager interface {
	AccessLists(string) (configuration.AccessLists, error)
	ApplyAccess(context.Context, string, string, configuration.AccessUpdateRequest) (configuration.ApplyResult, error)
}

type Service struct {
	rooms     RoomCatalog
	runtime   Runtime
	sender    Sender
	access    AccessManager
	store     *Store
	probe     Probe
	commander RuntimeCommander
	now       func() time.Time
	locksMu   sync.Mutex
	locks     map[string]*sync.Mutex
}

func NewService(roomCatalog RoomCatalog, runtime Runtime, sender Sender, access AccessManager, store *Store, probe Probe, commanders ...RuntimeCommander) (*Service, error) {
	if roomCatalog == nil || runtime == nil || sender == nil || access == nil || store == nil || probe == nil {
		return nil, errors.New("rooms, runtime, sender, access manager, store, and probe are required")
	}
	service := &Service{
		rooms: roomCatalog, runtime: runtime, sender: sender, access: access, store: store, probe: probe,
		now: time.Now, locks: make(map[string]*sync.Mutex),
	}
	if len(commanders) > 0 {
		service.commander = commanders[0]
	}
	return service, nil
}

func ValidID(value string) bool { return playerIDPattern.MatchString(strings.TrimSpace(value)) }

func (s *Service) roomLock(roomID string) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	if s.locks[roomID] == nil {
		s.locks[roomID] = &sync.Mutex{}
	}
	return s.locks[roomID]
}

func (s *Service) List(roomID string, filter ListFilter) (List, error) {
	room, err := s.managedRoom(roomID)
	if err != nil {
		return List{}, err
	}
	if err := s.expireRoomBans(room); err != nil {
		return List{}, err
	}
	filter, err = s.normalizeFilter(room.ID, filter)
	if err != nil {
		return List{}, err
	}
	items, total, err := s.store.List(room.ID, filter)
	if err != nil {
		return List{}, err
	}
	access, err := s.access.AccessLists(room.ID)
	if err != nil {
		return List{}, err
	}
	blocked := make(map[string]bool, len(access.Blocked))
	for _, id := range access.Blocked {
		blocked[id] = true
	}
	for index := range items {
		items[index].Banned = blocked[items[index].ID]
	}
	banDetails, err := s.store.Bans(room.ID)
	if err != nil {
		return List{}, err
	}
	for index := range items {
		if details, exists := banDetails[items[index].ID]; items[index].Banned && exists {
			applyBanDetails(&items[index], details)
		}
	}
	totalPlayers, online, refreshed, err := s.store.Counts(room.ID)
	if err != nil {
		return List{}, err
	}
	return List{
		Items: items, Total: total, Online: online, Offline: totalPlayers - online,
		Banned: len(access.Blocked), Limit: filter.Limit, Offset: filter.Offset, LastRefreshedAt: refreshed,
	}, nil
}

func (s *Service) Player(roomID, playerID string) (Player, error) {
	room, err := s.managedRoom(roomID)
	if err != nil {
		return Player{}, err
	}
	if err := s.expireRoomBans(room); err != nil {
		return Player{}, err
	}
	playerID = strings.TrimSpace(playerID)
	if !ValidID(playerID) {
		return Player{}, ErrInvalidPlayer
	}
	player, err := s.store.Get(room.ID, playerID)
	if err != nil {
		return Player{}, err
	}
	access, err := s.access.AccessLists(room.ID)
	if err != nil {
		return Player{}, err
	}
	player.Banned = contains(access.Blocked, player.ID)
	if player.Banned {
		banDetails, detailsErr := s.store.Bans(room.ID)
		if detailsErr != nil {
			return Player{}, detailsErr
		}
		if details, exists := banDetails[player.ID]; exists {
			applyBanDetails(&player, details)
		}
	}
	return player, nil
}

func (s *Service) WorldTargets(roomID string) ([]WorldTarget, error) {
	if _, err := s.managedRoom(roomID); err != nil {
		return nil, err
	}
	worlds, err := s.rooms.Worlds(roomID)
	if err != nil {
		return nil, err
	}
	targets := make([]WorldTarget, 0, len(worlds))
	for _, world := range worlds {
		targets = append(targets, WorldTarget{ID: world.ID, Name: world.Name})
	}
	return targets, nil
}

func (s *Service) AnyWorldRunning(ctx context.Context, roomID string, worldIDs []string) (bool, error) {
	room, err := s.managedRoom(roomID)
	if err != nil {
		return false, err
	}
	worlds, err := s.rooms.Worlds(room.ID)
	if err != nil {
		return false, err
	}
	selected := make(map[string]bool, len(worldIDs))
	for _, worldID := range worldIDs {
		selected[worldID] = true
	}
	found := len(selected) == 0
	for _, world := range worlds {
		if len(selected) > 0 && !selected[world.ID] {
			continue
		}
		found = true
		running, runningErr := s.worldRunning(ctx, room, world)
		if runningErr != nil {
			return false, runningErr
		}
		if running {
			return true, nil
		}
	}
	if !found {
		return false, rooms.ErrWorldNotFound
	}
	return false, nil
}

func (s *Service) RefreshWorld(ctx context.Context, roomID, worldID string) (RefreshResult, error) {
	outcomes, err := s.RefreshWorlds(ctx, roomID, []string{worldID})
	if err != nil {
		return RefreshResult{}, err
	}
	if len(outcomes) != 1 {
		return RefreshResult{}, errors.New("player refresh returned no world result")
	}
	return outcomes[0].Result, outcomes[0].Err
}

func (s *Service) RefreshWorlds(ctx context.Context, roomID string, worldIDs []string) ([]RefreshOutcome, error) {
	lock := s.roomLock(roomID)
	lock.Lock()
	defer lock.Unlock()
	room, err := s.managedRoom(roomID)
	if err != nil {
		return nil, err
	}
	if len(worldIDs) == 0 || len(worldIDs) > 64 {
		return nil, ErrInvalidFilter
	}
	seenWorlds := make(map[string]bool, len(worldIDs))
	outcomes := make([]RefreshOutcome, 0, len(worldIDs))
	snapshots := make([]worldSnapshot, 0, len(worldIDs))
	for _, worldID := range worldIDs {
		if seenWorlds[worldID] {
			return nil, ErrInvalidFilter
		}
		seenWorlds[worldID] = true
		outcome, snapshot := s.collectWorldSnapshot(ctx, room, worldID)
		outcomes = append(outcomes, outcome)
		if outcome.Err == nil {
			snapshots = append(snapshots, snapshot)
		}
	}
	if err := s.store.ReplaceRoomSnapshots(room.ID, snapshots); err != nil {
		return outcomes, err
	}
	return outcomes, nil
}

func (s *Service) collectWorldSnapshot(ctx context.Context, room rooms.Room, worldID string) (RefreshOutcome, worldSnapshot) {
	outcome := RefreshOutcome{WorldID: worldID}
	world, err := s.rooms.World(room.ID, worldID)
	if err != nil {
		outcome.Err = err
		return outcome, worldSnapshot{}
	}
	running, err := s.worldRunning(ctx, room, world)
	if err != nil {
		outcome.Err = err
		return outcome, worldSnapshot{}
	}
	observedAt := s.now().UTC()
	history, historyErr := s.readWorldHistory(ctx, room.ID, world.ID)
	snapshot := worldSnapshot{WorldID: world.ID, WorldName: world.Name, History: history, ObservedAt: observedAt, Source: SourceNativeLog}
	if !running {
		snapshot.Stopped = true
		message := "分片未运行，已确认该分片没有在线玩家"
		if len(history) > 0 {
			message = fmt.Sprintf("分片未运行，已恢复 %d 个历史玩家并标记为离线", len(history))
		}
		warning := ""
		if historyErr != nil {
			warning = "历史玩家日志读取失败：" + historyErr.Error()
		}
		outcome.Result = RefreshResult{WorldID: world.ID, Running: false, Source: SourceNativeLog, Status: FreshnessStale, Warning: warning, Message: message}
		return outcome, snapshot
	}
	observations, source, status, warning, snapshotAt, err := s.readSnapshot(ctx, room.ID, world.ID, observedAt)
	if err != nil {
		outcome.Err = err
		return outcome, worldSnapshot{}
	}
	observedAt = snapshotAt
	if err := validateObservations(observations); err != nil {
		outcome.Err = err
		return outcome, worldSnapshot{}
	}
	snapshot.Observations = observations
	snapshot.ObservedAt = observedAt
	snapshot.Source = source
	if historyErr != nil {
		if warning != "" {
			warning += "；"
		}
		warning += "历史玩家日志读取失败：" + historyErr.Error()
	}
	message := fmt.Sprintf("已通过 %s 读取 %d 个在线玩家", playerSourceLabel(source), len(observations))
	outcome.Result = RefreshResult{
		WorldID: world.ID, Count: len(observations), Running: true, Source: source, Status: status,
		ObservedAt: &observedAt, Warning: warning, Message: message,
	}
	return outcome, snapshot
}

func (s *Service) readSnapshot(ctx context.Context, roomID, worldID string, observedAt time.Time) ([]Observation, DataSource, FreshnessStatus, string, time.Time, error) {
	if detailed, ok := s.probe.(DetailedProbe); ok {
		result, err := detailed.SnapshotDetailed(ctx, roomID, worldID)
		if err != nil {
			return nil, "", FreshnessUnavailable, "", time.Time{}, err
		}
		status := FreshnessLive
		if result.Degraded {
			status = FreshnessStale
		}
		if result.ObservedAt.IsZero() {
			result.ObservedAt = observedAt
		}
		return result.Observations, result.Source, status, result.Warning, result.ObservedAt.UTC(), nil
	}
	observations, err := s.probe.Snapshot(ctx, roomID, worldID)
	if err != nil {
		return nil, "", FreshnessUnavailable, "", time.Time{}, err
	}
	observations = stampNativeObservations(observations, observedAt)
	return observations, SourceNativeLog, FreshnessLive, "", observedAt, nil
}

func playerSourceLabel(source DataSource) string {
	switch source {
	case SourceRuntime:
		return "customcommands"
	case SourceConsoleFallback:
		return "控制台 fallback"
	default:
		return "原生日志"
	}
}

func (s *Service) mergeWorldHistory(ctx context.Context, room rooms.Room, world rooms.World, observedAt time.Time) (int, error) {
	observations, err := s.readWorldHistory(ctx, room.ID, world.ID)
	if err != nil {
		return 0, err
	}
	return s.store.MergeWorldHistory(room.ID, world.ID, world.Name, observations, observedAt)
}

func (s *Service) readWorldHistory(ctx context.Context, roomID, worldID string) ([]Observation, error) {
	historyProbe, supported := s.probe.(HistoryProbe)
	if !supported {
		return []Observation{}, nil
	}
	observations, err := historyProbe.HistorySnapshot(ctx, roomID, worldID)
	if err != nil {
		return nil, err
	}
	if err := validateObservations(observations); err != nil {
		return nil, err
	}
	return observations, nil
}

func (s *Service) Act(ctx context.Context, jobID, roomID, playerID string, action Action, request ActionRequest) (ActionResult, error) {
	lock := s.roomLock(roomID)
	lock.Lock()
	defer lock.Unlock()
	room, err := s.managedRoom(roomID)
	if err != nil {
		return ActionResult{}, err
	}
	playerID = strings.TrimSpace(playerID)
	if !ValidID(playerID) {
		return ActionResult{}, ErrInvalidPlayer
	}
	player, err := s.store.Get(room.ID, playerID)
	if err != nil {
		return ActionResult{}, err
	}
	request.WorldID = strings.TrimSpace(request.WorldID)
	if request.WorldID == "" {
		request.WorldID = player.WorldID
	}
	world, err := s.rooms.World(room.ID, request.WorldID)
	if err != nil {
		return ActionResult{}, err
	}
	if err := validateAction(room, player, action, request); err != nil {
		return ActionResult{}, err
	}
	result := ActionResult{PlayerID: player.ID, WorldID: world.ID, Action: action}
	runtimeRequest, runtimeSupported := runtimePlayerRequest(jobID, action, player, request)
	switch action {
	case ActionKick:
		if err := s.sendPlayerAction(ctx, room, world, runtimeRequest, runtimeSupported, `TheNet:Kick(`+quoteLua(player.ID)+`)`); err != nil {
			return ActionResult{}, err
		}
		_ = s.store.MarkPlayerOffline(room.ID, player.ID, s.now().UTC())
		result.Message = "已向目标分片发送踢出命令"
	case ActionAnnounce:
		message := "[给 " + player.Name + "] " + strings.TrimSpace(request.Message)
		runtimeRequest.Arguments["message"] = message
		if err := s.sendPlayerAction(ctx, room, world, runtimeRequest, runtimeSupported, `c_announce(`+quoteLua(message)+`)`); err != nil {
			return ActionResult{}, err
		}
		result.Message = "已向玩家所在分片广播提醒"
	case ActionBan, ActionUnban:
		blocked := action == ActionBan
		backupID, changed, err := s.updateBlocklist(ctx, jobID, room, player.ID, blocked, request.Confirmation)
		if err != nil {
			return ActionResult{}, err
		}
		result.ProtectionBackupID = backupID
		if blocked {
			expiresAt, expiryErr := banExpiry(s.now().UTC(), request.Duration)
			if expiryErr != nil {
				return ActionResult{}, expiryErr
			}
			ban := Ban{
				RoomID: room.ID, PlayerID: player.ID, Reason: strings.TrimSpace(request.Reason),
				Duration: request.Duration, CreatedAt: s.now().UTC(), ExpiresAt: expiresAt,
			}
			if saveErr := s.store.SaveBan(ban); saveErr != nil {
				if changed {
					_, _, rollbackErr := s.updateBlocklist(ctx, uuid.NewString(), room, player.ID, false, room.Name)
					return ActionResult{}, errors.Join(saveErr, rollbackErr)
				}
				return ActionResult{}, saveErr
			}
		} else if deleteErr := s.store.DeleteBan(room.ID, player.ID); deleteErr != nil {
			result.Warning = "封禁名单已更新，但封禁说明清理失败：" + deleteErr.Error()
		}
		script := `TheNet:Ban(` + quoteLua(player.ID) + `)`
		result.Message = "玩家已加入封禁名单"
		if !blocked {
			script = `TheNet:Unban(` + quoteLua(player.ID) + `)`
			result.Message = "玩家已从封禁名单移除"
		}
		running, runtimeErr := s.worldRunning(ctx, room, world)
		if runtimeErr != nil {
			result.Warning = "名单已保存，但无法确认分片状态：" + runtimeErr.Error()
		} else if running {
			if sendErr := s.sendPlayerAction(ctx, room, world, runtimeRequest, runtimeSupported, script); sendErr != nil {
				result.Warning = "名单已保存，但即时命令发送失败：" + sendErr.Error()
			}
		}
		if blocked {
			_ = s.store.MarkPlayerOffline(room.ID, player.ID, s.now().UTC())
		}
		if !changed {
			result.Message += "（名单原本已是目标状态）"
		}
	case ActionKill:
		if err := s.sendPlayerAction(ctx, room, world, runtimeRequest, runtimeSupported, playerLookupScript(player.ID, `p:PushEvent("death")`)); err != nil {
			return ActionResult{}, err
		}
		result.Message = "已向目标分片发送玩家死亡命令"
	case ActionGodMode:
		enabled := *request.Enabled
		statement := `if p.components.health then p.components.health:SetInvincible(` + luaBoolean(enabled) + `) end; ` +
			`if p.components.talker then p.components.talker:Say(` + quoteLua(toggleMessage("无敌模式", enabled)) + `) end`
		if err := s.sendPlayerAction(ctx, room, world, runtimeRequest, runtimeSupported, playerLookupScript(player.ID, statement)); err != nil {
			return ActionResult{}, err
		}
		result.Message = "玩家无敌模式已" + enabledText(enabled)
	case ActionCreativeMode:
		enabled := *request.Enabled
		statement := `if p.components.builder then p.components.builder.freebuildmode=` + luaBoolean(enabled) + ` end; ` +
			`if p.components.talker then p.components.talker:Say(` + quoteLua(toggleMessage("制作模式", enabled)) + `) end`
		if err := s.sendPlayerAction(ctx, room, world, runtimeRequest, runtimeSupported, playerLookupScript(player.ID, statement)); err != nil {
			return ActionResult{}, err
		}
		result.Message = "玩家制作模式已" + enabledText(enabled)
	case ActionResurrect:
		statement := `p:PushEvent("respawnfromghost"); p.rezsource=` + quoteLua("DST-ADMIN-GO控制台")
		if err := s.sendPlayerAction(ctx, room, world, runtimeRequest, runtimeSupported, playerLookupScript(player.ID, statement)); err != nil {
			return ActionResult{}, err
		}
		result.Message = "已向目标分片发送玩家复活命令"
	case ActionChangeCharacter:
		statement := `c_despawn(p); c_announce(` + quoteLua("管理员已将玩家重置，该玩家可以重新选择角色") + `)`
		if err := s.sendPlayerAction(ctx, room, world, runtimeRequest, runtimeSupported, playerLookupScript(player.ID, statement)); err != nil {
			return ActionResult{}, err
		}
		_ = s.store.MarkPlayerOffline(room.ID, player.ID, s.now().UTC())
		result.Message = "已向目标分片发送重选人物命令"
	default:
		return ActionResult{}, ErrInvalidAction
	}
	return result, nil
}

func (s *Service) updateBlocklist(ctx context.Context, jobID string, room rooms.Room, playerID string, blocked bool, confirmation string) (string, bool, error) {
	current, err := s.access.AccessLists(room.ID)
	if err != nil {
		return "", false, err
	}
	next := append([]string(nil), current.Blocked...)
	present := contains(next, playerID)
	if blocked && !present {
		next = append(next, playerID)
		sort.Strings(next)
	} else if !blocked && present {
		filtered := next[:0]
		for _, id := range next {
			if id != playerID {
				filtered = append(filtered, id)
			}
		}
		next = filtered
	} else {
		return "", false, nil
	}
	apply, err := s.access.ApplyAccess(ctx, jobID, room.ID, configuration.AccessUpdateRequest{
		ExpectedRevision: current.Revision, Admins: current.Admins, Blocked: next,
		Whitelist: current.Whitelist, Confirmation: confirmation,
	})
	if err != nil {
		return "", false, err
	}
	return apply.ProtectionBackupID, true, nil
}

func (s *Service) sendToRunningWorld(ctx context.Context, room rooms.Room, world rooms.World, script string) error {
	running, err := s.worldRunning(ctx, room, world)
	if err != nil {
		return err
	}
	if !running {
		return ErrWorldNotRunning
	}
	return s.sender.Send(ctx, room.DirectoryName, world.DirectoryName, markedPlayerScript("action", "", script))
}

func (s *Service) worldRunning(ctx context.Context, room rooms.Room, world rooms.World) (bool, error) {
	if runtime, ok := s.runtime.(identifiedRuntime); ok {
		status, err := runtime.StatusFor(ctx, room.ID, world.ID)
		return status.State == shards.RuntimeRunning, err
	}
	return s.runtime.IsRunning(ctx, room.DirectoryName, world.DirectoryName)
}

func (s *Service) sendPlayerAction(ctx context.Context, room rooms.Room, world rooms.World, request dstruntime.CommandRequest, runtimeSupported bool, fallback string) error {
	if s.commander != nil && runtimeSupported {
		receipt, err := s.commander.ExecuteCommand(ctx, room.ID, world.ID, request)
		if err == nil {
			if receipt.OK {
				return nil
			}
			return fmt.Errorf("runtime action rejected: %s: %s", receipt.Code, receipt.Message)
		}
		if !errors.Is(err, dstruntime.ErrRuntimeUnavailable) && !errors.Is(err, dstruntime.ErrRuntimeNotInstalled) {
			return err
		}
	}
	return s.sendToRunningWorld(ctx, room, world, fallback)
}

func runtimePlayerRequest(requestID string, action Action, player Player, request ActionRequest) (dstruntime.CommandRequest, bool) {
	actions := map[Action]string{
		ActionKick: "player.kick", ActionBan: "player.ban", ActionUnban: "player.unban", ActionAnnounce: "player.announce",
		ActionKill: "player.kill", ActionGodMode: "player.god_mode", ActionCreativeMode: "player.creative_mode",
		ActionResurrect: "player.resurrect", ActionChangeCharacter: "player.change_character",
	}
	runtimeAction, supported := actions[action]
	arguments := map[string]interface{}{"userId": player.ID}
	if request.Enabled != nil {
		arguments["enabled"] = *request.Enabled
	}
	return dstruntime.CommandRequest{RequestID: requestID, Action: runtimeAction, Arguments: arguments}, supported
}

func (s *Service) managedRoom(roomID string) (rooms.Room, error) {
	room, err := s.rooms.Room(roomID)
	if err != nil {
		return rooms.Room{}, err
	}
	if !room.Managed {
		return rooms.Room{}, ErrRoomNotManaged
	}
	return room, nil
}

func (s *Service) normalizeFilter(roomID string, filter ListFilter) (ListFilter, error) {
	filter.Query = strings.TrimSpace(filter.Query)
	filter.Prefab = strings.TrimSpace(filter.Prefab)
	filter.WorldID = strings.TrimSpace(filter.WorldID)
	if len([]rune(filter.Query)) > 128 || len(filter.Prefab) > 128 || (filter.Status != "" && filter.Status != "online" && filter.Status != "offline") {
		return ListFilter{}, ErrInvalidFilter
	}
	if filter.WorldID != "" {
		if _, err := s.rooms.World(roomID, filter.WorldID); err != nil {
			return ListFilter{}, err
		}
	}
	if filter.Limit <= 0 || filter.Limit > 100 {
		filter.Limit = 25
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	return filter, nil
}

func (s *Service) StartBanExpiryScheduler(ctx context.Context, interval time.Duration) {
	go s.RunBanExpiryScheduler(ctx, interval)
}

func (s *Service) RunBanExpiryScheduler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	_ = s.ExpireBans(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.ExpireBans(ctx)
		}
	}
}

func (s *Service) ExpireBans(ctx context.Context) error {
	expired, err := s.store.ExpiredBans(s.now().UTC())
	if err != nil {
		return err
	}
	var result error
	for _, ban := range expired {
		room, roomErr := s.managedRoom(ban.RoomID)
		if roomErr != nil {
			result = errors.Join(result, roomErr)
			continue
		}
		if expireErr := s.expireBan(ctx, room, ban); expireErr != nil {
			result = errors.Join(result, expireErr)
		}
	}
	return result
}

func (s *Service) expireRoomBans(room rooms.Room) error {
	expired, err := s.store.ExpiredBans(s.now().UTC())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var result error
	for _, ban := range expired {
		if ban.RoomID != room.ID {
			continue
		}
		if expireErr := s.expireBan(ctx, room, ban); expireErr != nil {
			result = errors.Join(result, expireErr)
		}
	}
	return result
}

func (s *Service) expireBan(ctx context.Context, room rooms.Room, ban Ban) error {
	lock := s.roomLock(room.ID)
	lock.Lock()
	defer lock.Unlock()
	if ban.ExpiresAt == nil || ban.ExpiresAt.After(s.now().UTC()) {
		return nil
	}
	if _, _, err := s.updateBlocklist(ctx, uuid.NewString(), room, ban.PlayerID, false, room.Name); err != nil {
		return err
	}
	return s.store.DeleteBan(room.ID, ban.PlayerID)
}

func validateAction(room rooms.Room, player Player, action Action, request ActionRequest) error {
	fields := make(map[string]string)
	switch action {
	case ActionKick:
		if request.Confirmation != player.ID {
			return ErrConfirmationRequired
		}
		if !player.Online {
			fields["playerId"] = "只能踢出当前在线玩家"
		}
		if player.Online && request.WorldID != player.WorldID {
			fields["worldId"] = "目标世界与玩家当前世界不一致"
		}
	case ActionBan:
		if request.Confirmation != room.Name {
			return ErrConfirmationRequired
		}
		reason := strings.TrimSpace(request.Reason)
		if reason == "" || len([]rune(reason)) > 300 || !utf8.ValidString(reason) || strings.ContainsRune(reason, '\x00') {
			fields["reason"] = "封禁原因必须为 1-300 个有效字符"
		}
		if _, err := banExpiry(time.Now(), request.Duration); err != nil {
			fields["duration"] = "封禁时长无效"
		}
	case ActionUnban:
		if request.Confirmation != room.Name {
			return ErrConfirmationRequired
		}
	case ActionAnnounce:
		message := strings.TrimSpace(request.Message)
		if message == "" || len([]rune(message)) > 300 || !utf8.ValidString(message) || strings.ContainsRune(message, '\x00') {
			fields["message"] = "消息必须为 1-300 个有效字符"
		}
		if !player.Online {
			fields["playerId"] = "只能向当前在线玩家所在分片广播"
		}
		if player.Online && request.WorldID != player.WorldID {
			fields["worldId"] = "目标世界与玩家当前世界不一致"
		}
	case ActionKill, ActionResurrect, ActionChangeCharacter:
		if request.Confirmation != player.ID {
			return ErrConfirmationRequired
		}
		validateOnlinePlayerWorld(fields, player, request.WorldID)
	case ActionGodMode, ActionCreativeMode:
		if request.Enabled == nil {
			fields["enabled"] = "必须明确指定开启或关闭"
		}
		validateOnlinePlayerWorld(fields, player, request.WorldID)
	default:
		return ErrInvalidAction
	}
	if len(fields) > 0 {
		return &FieldError{Fields: fields}
	}
	return nil
}

func validateOnlinePlayerWorld(fields map[string]string, player Player, worldID string) {
	if !player.Online {
		fields["playerId"] = "只能操作当前在线玩家"
	}
	if player.Online && worldID != player.WorldID {
		fields["worldId"] = "目标世界与玩家当前世界不一致"
	}
}

func banExpiry(now time.Time, duration string) (*time.Time, error) {
	durations := map[string]time.Duration{
		"1h": time.Hour, "6h": 6 * time.Hour, "12h": 12 * time.Hour,
		"1d": 24 * time.Hour, "3d": 3 * 24 * time.Hour, "7d": 7 * 24 * time.Hour,
		"30d": 30 * 24 * time.Hour,
	}
	if duration == "permanent" {
		return nil, nil
	}
	value, exists := durations[duration]
	if !exists {
		return nil, ErrInvalidAction
	}
	expiresAt := now.Add(value).UTC()
	return &expiresAt, nil
}

func applyBanDetails(player *Player, ban Ban) {
	createdAt := ban.CreatedAt.UTC()
	player.BanReason = ban.Reason
	player.BannedAt = &createdAt
	player.BanExpiresAt = utcTimePointer(ban.ExpiresAt)
}

func utcTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func playerLookupScript(playerID, statement string) string {
	return `local p=UserToPlayer(` + quoteLua(playerID) + `); if p ~= nil then ` + statement + ` end`
}

func luaBoolean(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func enabledText(value bool) string {
	if value {
		return "开启"
	}
	return "关闭"
}

func toggleMessage(name string, enabled bool) string {
	return name + "已" + enabledText(enabled)
}

func validateObservations(values []Observation) error {
	if len(values) > 64 {
		return errors.New("player snapshot exceeds room limit")
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !ValidID(value.ID) || seen[value.ID] || value.Name == "" || len([]rune(value.Name)) > 256 || len(value.Prefab) > 128 || value.Age < 0 {
			return errors.New("player snapshot contains invalid data")
		}
		seen[value.ID] = true
	}
	return nil
}

func markedPlayerScript(action interface{}, playerID, script string) string {
	marker := fmt.Sprintf("[DST-ADMIN-PLAYER-ACTION %v %s]", action, playerID)
	return `print(` + quoteLua(marker+" START") + `); ` + script + `; print(` + quoteLua(marker+" DONE") + `)`
}

func quoteLua(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, character := range value {
		switch character {
		case '\\':
			builder.WriteString(`\\`)
		case '"':
			builder.WriteString(`\"`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			builder.WriteRune(character)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
