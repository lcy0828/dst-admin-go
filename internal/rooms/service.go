package rooms

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-ini/ini"
)

var directoryNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

type CreateRequest struct {
	DirectoryName string `json:"directoryName"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	GameMode      string `json:"gameMode"`
	MaxPlayers    int    `json:"maxPlayers"`
	PvP           bool   `json:"pvp"`
	Password      string `json:"password"`
	ClusterToken  string `json:"clusterToken"`
	IncludeCaves  bool   `json:"includeCaves"`
}

type CreateWorldRequest struct {
	DirectoryName string `json:"directoryName"`
	Type          string `json:"type"`
}

type DeleteWorldRequest struct {
	Confirmation string `json:"confirmation"`
}

type DeleteWorldResult struct {
	World        World  `json:"world"`
	RecoveryName string `json:"recoveryName"`
}

type Service struct {
	catalog *Catalog
	store   *Store
	worldMu sync.Mutex
}

func NewService(catalog *Catalog, store *Store) *Service {
	return &Service{catalog: catalog, store: store}
}

func (s *Service) List() ([]Room, error) { return s.catalog.List() }

func (s *Service) Room(roomID string) (Room, error) { return s.catalog.Room(roomID) }

func (s *Service) Worlds(roomID string) ([]World, error) { return s.catalog.Worlds(roomID) }

func (s *Service) World(roomID, worldID string) (World, error) {
	return s.catalog.World(roomID, worldID)
}

func (s *Service) Adopt(roomID string) (Room, error) {
	room, err := s.catalog.Room(roomID)
	if err != nil {
		return Room{}, err
	}
	if err := s.store.Adopt(room); err != nil {
		return Room{}, err
	}
	room.Managed = true
	return room, nil
}

func (s *Service) Create(request CreateRequest) (Room, error) {
	request.DirectoryName = strings.TrimSpace(request.DirectoryName)
	request.Name = strings.TrimSpace(request.Name)
	request.Description = strings.TrimSpace(request.Description)
	request.GameMode = strings.TrimSpace(strings.ToLower(request.GameMode))
	if err := validateCreateRequest(request); err != nil {
		return Room{}, err
	}
	if err := os.MkdirAll(s.catalog.root, 0750); err != nil {
		return Room{}, fmt.Errorf("create save root: %w", err)
	}
	target := filepath.Join(s.catalog.root, request.DirectoryName)
	if err := ensureContained(s.catalog.root, target); err != nil {
		return Room{}, err
	}
	if _, err := os.Lstat(target); err == nil {
		return Room{}, ErrRoomExists
	} else if !os.IsNotExist(err) {
		return Room{}, fmt.Errorf("inspect room target: %w", err)
	}
	temporary, err := os.MkdirTemp(s.catalog.root, ".dst-admin-create-")
	if err != nil {
		return Room{}, fmt.Errorf("create room staging directory: %w", err)
	}
	published := false
	completed := false
	defer func() {
		if !published {
			_ = os.RemoveAll(temporary)
		} else if !completed {
			_ = os.RemoveAll(target)
		}
	}()
	if err := writeRoomFiles(temporary, request); err != nil {
		return Room{}, err
	}
	if err := os.Rename(temporary, target); err != nil {
		return Room{}, fmt.Errorf("publish room directory: %w", err)
	}
	published = true
	room, err := s.catalog.Room(EncodeID(request.DirectoryName))
	if err != nil {
		return Room{}, err
	}
	if err := s.store.Adopt(room); err != nil {
		return Room{}, err
	}
	room.Managed = true
	completed = true
	return room, nil
}

func (s *Service) CreateWorld(roomID string, request CreateWorldRequest) (World, error) {
	s.worldMu.Lock()
	defer s.worldMu.Unlock()

	request.DirectoryName = strings.TrimSpace(request.DirectoryName)
	request.Type = strings.ToLower(strings.TrimSpace(request.Type))
	fields := make(map[string]string)
	if !directoryNamePattern.MatchString(request.DirectoryName) {
		fields["directoryName"] = "仅允许 1-64 位字母、数字、下划线和短横线，且必须以字母或数字开头"
	}
	if request.Type != "forest" && request.Type != "cave" {
		fields["type"] = "世界类型必须为 forest 或 cave"
	}
	if len(fields) > 0 {
		return World{}, &ValidationError{Fields: fields}
	}
	room, err := s.catalog.Room(roomID)
	if err != nil {
		return World{}, err
	}
	if !room.Managed {
		return World{}, ErrRoomNotManaged
	}
	roomPath := filepath.Join(s.catalog.root, room.DirectoryName)
	target := filepath.Join(roomPath, request.DirectoryName)
	if err := ensureContained(roomPath, target); err != nil {
		return World{}, err
	}
	if _, err := os.Lstat(target); err == nil {
		return World{}, ErrWorldExists
	} else if !os.IsNotExist(err) {
		return World{}, fmt.Errorf("inspect world target: %w", err)
	}
	allocation, err := s.nextWorldAllocation(roomID, roomPath, request.Type)
	if err != nil {
		return World{}, err
	}
	temporary, err := os.MkdirTemp(roomPath, ".dst-admin-create-world-")
	if err != nil {
		return World{}, fmt.Errorf("create world staging directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	if err := writeWorld(
		temporary,
		request.DirectoryName,
		allocation.shardID,
		allocation.serverPort,
		allocation.authenticationPort,
		allocation.masterServerPort,
		allocation.master,
		request.Type,
	); err != nil {
		return World{}, err
	}
	if err := os.Rename(filepath.Join(temporary, request.DirectoryName), target); err != nil {
		return World{}, fmt.Errorf("publish world directory: %w", err)
	}
	return s.catalog.World(roomID, EncodeID(request.DirectoryName))
}

func (s *Service) DeleteWorld(roomID, worldID string, request DeleteWorldRequest) (DeleteWorldResult, error) {
	s.worldMu.Lock()
	defer s.worldMu.Unlock()

	room, err := s.catalog.Room(roomID)
	if err != nil {
		return DeleteWorldResult{}, err
	}
	if !room.Managed {
		return DeleteWorldResult{}, ErrRoomNotManaged
	}
	if request.Confirmation != room.Name {
		return DeleteWorldResult{}, ErrConfirmation
	}
	world, err := s.catalog.World(roomID, worldID)
	if err != nil {
		return DeleteWorldResult{}, err
	}
	roomPath := filepath.Join(s.catalog.root, room.DirectoryName)
	source := filepath.Join(roomPath, world.DirectoryName)
	trashRoot := filepath.Join(roomPath, ".dst-admin-trash")
	if err := ensureContained(roomPath, source); err != nil {
		return DeleteWorldResult{}, err
	}
	if err := os.MkdirAll(trashRoot, 0750); err != nil {
		return DeleteWorldResult{}, fmt.Errorf("create world recovery directory: %w", err)
	}
	trashName := strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + world.DirectoryName
	target := filepath.Join(trashRoot, trashName)
	if err := os.Rename(source, target); err != nil {
		return DeleteWorldResult{}, fmt.Errorf("move world to recovery directory: %w", err)
	}
	return DeleteWorldResult{World: world, RecoveryName: filepath.Join(".dst-admin-trash", trashName)}, nil
}

type worldAllocation struct {
	shardID            int
	serverPort         int
	authenticationPort int
	masterServerPort   int
	master             bool
}

func (s *Service) nextWorldAllocation(roomID, roomPath, worldType string) (worldAllocation, error) {
	worlds, err := s.catalog.Worlds(roomID)
	if err != nil {
		return worldAllocation{}, err
	}
	usedShardIDs := make(map[int]bool)
	usedPorts := make(map[int]bool)
	hasMaster := false
	for _, world := range worlds {
		config, loadErr := ini.Load(filepath.Join(roomPath, world.DirectoryName, "server.ini"))
		if loadErr != nil {
			return worldAllocation{}, fmt.Errorf("parse existing world server.ini: %w", loadErr)
		}
		shardID := config.Section("SHARD").Key("id").MustInt(0)
		if shardID > 0 {
			usedShardIDs[shardID] = true
		}
		for _, port := range []int{
			config.Section("NETWORK").Key("server_port").MustInt(0),
			config.Section("STEAM").Key("authentication_port").MustInt(0),
			config.Section("STEAM").Key("master_server_port").MustInt(0),
		} {
			if port > 0 {
				usedPorts[port] = true
			}
		}
		hasMaster = hasMaster || world.IsMaster
	}
	master := worldType == "forest" && !hasMaster
	shardID := 2
	if master {
		shardID = 1
	}
	for usedShardIDs[shardID] {
		shardID++
	}
	serverPort := nextFreePort(10998+shardID, usedPorts)
	usedPorts[serverPort] = true
	authenticationPort := nextFreePort(8766+shardID, usedPorts)
	usedPorts[authenticationPort] = true
	masterServerPort := nextFreePort(27016+shardID, usedPorts)
	return worldAllocation{
		shardID: shardID, serverPort: serverPort, authenticationPort: authenticationPort,
		masterServerPort: masterServerPort, master: master,
	}, nil
}

func nextFreePort(candidate int, used map[int]bool) int {
	for candidate <= 65535 && used[candidate] {
		candidate++
	}
	return candidate
}

func validateCreateRequest(request CreateRequest) error {
	details := make(map[string]string)
	if !directoryNamePattern.MatchString(request.DirectoryName) {
		details["directoryName"] = "仅允许 1-64 位字母、数字、下划线和短横线，且必须以字母或数字开头"
	}
	if request.Name == "" || len([]rune(request.Name)) > 64 {
		details["name"] = "房间名称必须为 1-64 个字符"
	}
	if len([]rune(request.Description)) > 512 {
		details["description"] = "房间描述不能超过 512 个字符"
	}
	if request.MaxPlayers < 1 || request.MaxPlayers > 64 {
		details["maxPlayers"] = "玩家上限必须在 1-64 之间"
	}
	validModes := map[string]bool{"survival": true, "endless": true, "wilderness": true}
	if !validModes[request.GameMode] {
		details["gameMode"] = "仅支持 survival、endless 或 wilderness"
	}
	if len([]rune(request.Password)) > 64 || strings.ContainsAny(request.Password, "\x00\r\n") {
		details["password"] = "密码不能超过 64 个字符且不能包含换行"
	}
	if len(request.ClusterToken) > 4096 || strings.ContainsRune(request.ClusterToken, '\x00') {
		details["clusterToken"] = "令牌格式无效"
	}
	if len(details) > 0 {
		return &ValidationError{Fields: details}
	}
	return nil
}

type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string { return "room input is invalid" }

func writeRoomFiles(root string, request CreateRequest) error {
	clusterKey, err := randomClusterKey()
	if err != nil {
		return err
	}
	cluster := ini.Empty()
	gameplay, _ := cluster.NewSection("GAMEPLAY")
	_, _ = gameplay.NewKey("game_mode", request.GameMode)
	_, _ = gameplay.NewKey("max_players", fmt.Sprintf("%d", request.MaxPlayers))
	_, _ = gameplay.NewKey("pvp", fmt.Sprintf("%t", request.PvP))
	network, _ := cluster.NewSection("NETWORK")
	_, _ = network.NewKey("cluster_name", request.Name)
	_, _ = network.NewKey("cluster_description", request.Description)
	_, _ = network.NewKey("cluster_password", request.Password)
	misc, _ := cluster.NewSection("MISC")
	_, _ = misc.NewKey("console_enabled", "true")
	shard, _ := cluster.NewSection("SHARD")
	_, _ = shard.NewKey("shard_enabled", "true")
	_, _ = shard.NewKey("bind_ip", "127.0.0.1")
	_, _ = shard.NewKey("master_ip", "127.0.0.1")
	_, _ = shard.NewKey("master_port", "10889")
	_, _ = shard.NewKey("cluster_key", clusterKey)
	if err := writeINI(filepath.Join(root, "cluster.ini"), cluster, 0640); err != nil {
		return err
	}
	if token := strings.TrimSpace(request.ClusterToken); token != "" {
		if err := os.WriteFile(filepath.Join(root, "cluster_token.txt"), []byte(token+"\n"), 0600); err != nil {
			return fmt.Errorf("write cluster token: %w", err)
		}
	}
	if err := writeWorld(root, "Master", 1, 10999, 8767, 27017, true, "forest"); err != nil {
		return err
	}
	if request.IncludeCaves {
		if err := writeWorld(root, "Caves", 2, 11000, 8768, 27018, false, "cave"); err != nil {
			return err
		}
	}
	return nil
}

func randomClusterKey() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate shard cluster key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func writeWorld(root, name string, shardID, port, authenticationPort, masterServerPort int, master bool, location string) error {
	directory := filepath.Join(root, name)
	if err := os.MkdirAll(directory, 0750); err != nil {
		return fmt.Errorf("create %s world: %w", name, err)
	}
	config := ini.Empty()
	network, _ := config.NewSection("NETWORK")
	_, _ = network.NewKey("server_port", fmt.Sprintf("%d", port))
	shard, _ := config.NewSection("SHARD")
	_, _ = shard.NewKey("is_master", fmt.Sprintf("%t", master))
	_, _ = shard.NewKey("name", name)
	_, _ = shard.NewKey("id", strconv.Itoa(shardID))
	steam, _ := config.NewSection("STEAM")
	_, _ = steam.NewKey("authentication_port", strconv.Itoa(authenticationPort))
	_, _ = steam.NewKey("master_server_port", strconv.Itoa(masterServerPort))
	account, _ := config.NewSection("ACCOUNT")
	_, _ = account.NewKey("encode_user_path", "true")
	if err := writeINI(filepath.Join(directory, "server.ini"), config, 0640); err != nil {
		return err
	}
	override := fmt.Sprintf(`return {
  desc = "The standard Don't Starve Together experience.",
  hideminimap = false,
  id = "SURVIVAL_TOGETHER",
  location = %q,
  max_playlist_position = 999,
  min_playlist_position = 0,
  name = "Default",
  numrandom_set_pieces = 4,
  override_enabled = true,
  overrides = {},
  playstyle = "survival",
  random_set_pieces = {},
  required_prefabs = {},
  required_setpieces = {},
  substitutes = {},
  version = 4,
}
`, location)
	if err := os.WriteFile(filepath.Join(directory, "leveldataoverride.lua"), []byte(override), 0640); err != nil {
		return fmt.Errorf("write %s world override: %w", name, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "modoverrides.lua"), []byte("return {}\n"), 0640); err != nil {
		return fmt.Errorf("write %s mod overrides: %w", name, err)
	}
	return nil
}

func writeINI(path string, config *ini.File, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	_, writeErr := config.WriteTo(file)
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), writeErr)
	}
	if closeErr != nil && !errors.Is(closeErr, io.ErrClosedPipe) {
		return fmt.Errorf("close %s: %w", filepath.Base(path), closeErr)
	}
	return nil
}
