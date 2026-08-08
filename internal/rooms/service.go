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
	"strings"

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

type Service struct {
	catalog *Catalog
	store   *Store
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
	if err := writeWorld(root, "Master", 10999, true, "forest"); err != nil {
		return err
	}
	if request.IncludeCaves {
		if err := writeWorld(root, "Caves", 11000, false, "cave"); err != nil {
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

func writeWorld(root, name string, port int, master bool, location string) error {
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
	_, _ = shard.NewKey("id", fmt.Sprintf("%d", map[bool]int{true: 1, false: 2}[master]))
	_, _ = config.NewSection("STEAM")
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
