package rooms

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-ini/ini"
)

var (
	ErrInvalidID       = errors.New("invalid room or world id")
	ErrUnsafePath      = errors.New("unsafe room or world path")
	ErrRoomNotFound    = errors.New("room not found")
	ErrWorldNotFound   = errors.New("world not found")
	ErrInvalidRoom     = errors.New("room configuration is invalid")
	ErrInvalidWorld    = errors.New("world configuration is invalid")
	ErrSaveRootMissing = errors.New("save root does not exist")
	ErrRoomExists      = errors.New("room already exists")
	ErrWorldExists     = errors.New("world already exists")
	ErrRoomNotManaged  = errors.New("room must be adopted before it can be changed")
	ErrConfirmation    = errors.New("exact room name confirmation is required")
	ErrWorldRunning    = errors.New("world must be stopped before it can be changed")
)

type Room struct {
	ID                string    `json:"id"`
	DirectoryName     string    `json:"directoryName"`
	Name              string    `json:"name"`
	Description       string    `json:"description"`
	GameMode          string    `json:"gameMode"`
	MaxPlayers        int       `json:"maxPlayers"`
	PvP               bool      `json:"pvp"`
	PasswordProtected bool      `json:"passwordProtected"`
	Managed           bool      `json:"managed"`
	WorldCount        int       `json:"worldCount"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

type WorldRole string

const (
	WorldRoleMaster WorldRole = "master"
	WorldRoleCaves  WorldRole = "caves"
	WorldRoleCustom WorldRole = "custom"
)

type World struct {
	ID            string    `json:"id"`
	RoomID        string    `json:"roomId"`
	DirectoryName string    `json:"directoryName"`
	Name          string    `json:"name"`
	Role          WorldRole `json:"role"`
	IsMaster      bool      `json:"isMaster"`
	ServerPort    int       `json:"serverPort"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type ManagedRooms interface {
	IsManaged(roomID string) (bool, error)
}

type Catalog struct {
	root    string
	managed ManagedRooms
}

func NewCatalog(root string, managed ManagedRooms) (*Catalog, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("save root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve save root: %w", err)
	}
	return &Catalog{root: filepath.Clean(absolute), managed: managed}, nil
}

func (c *Catalog) Root() string { return c.root }

func (c *Catalog) List() ([]Room, error) {
	entries, err := os.ReadDir(c.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrSaveRootMissing
		}
		return nil, fmt.Errorf("read save root: %w", err)
	}
	rooms := make([]Room, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		room, err := c.roomFromName(entry.Name())
		if errors.Is(err, ErrInvalidRoom) || errors.Is(err, ErrUnsafePath) {
			continue
		}
		if err != nil {
			return nil, err
		}
		rooms = append(rooms, room)
	}
	sort.Slice(rooms, func(i, j int) bool {
		return strings.ToLower(rooms[i].Name) < strings.ToLower(rooms[j].Name)
	})
	return rooms, nil
}

func (c *Catalog) Room(roomID string) (Room, error) {
	name, err := DecodeID(roomID)
	if err != nil {
		return Room{}, err
	}
	return c.roomFromName(name)
}

func (c *Catalog) Worlds(roomID string) ([]World, error) {
	roomName, err := DecodeID(roomID)
	if err != nil {
		return nil, err
	}
	roomPath, err := c.existingDirectory(c.root, roomName, ErrRoomNotFound)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(roomPath, "cluster.ini")); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrInvalidRoom
		}
		return nil, fmt.Errorf("inspect cluster.ini: %w", err)
	}
	entries, err := os.ReadDir(roomPath)
	if err != nil {
		return nil, fmt.Errorf("read room directory: %w", err)
	}
	worlds := make([]World, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		world, err := c.worldFromName(roomID, roomPath, entry.Name())
		if errors.Is(err, ErrInvalidWorld) || errors.Is(err, ErrUnsafePath) {
			continue
		}
		if err != nil {
			return nil, err
		}
		worlds = append(worlds, world)
	}
	sort.SliceStable(worlds, func(i, j int) bool {
		if worlds[i].Role != worlds[j].Role {
			return worldRoleOrder(worlds[i].Role) < worldRoleOrder(worlds[j].Role)
		}
		return strings.ToLower(worlds[i].Name) < strings.ToLower(worlds[j].Name)
	})
	return worlds, nil
}

func (c *Catalog) World(roomID, worldID string) (World, error) {
	roomName, err := DecodeID(roomID)
	if err != nil {
		return World{}, err
	}
	worldName, err := DecodeID(worldID)
	if err != nil {
		return World{}, err
	}
	roomPath, err := c.existingDirectory(c.root, roomName, ErrRoomNotFound)
	if err != nil {
		return World{}, err
	}
	return c.worldFromName(roomID, roomPath, worldName)
}

func (c *Catalog) roomFromName(directoryName string) (Room, error) {
	roomPath, err := c.existingDirectory(c.root, directoryName, ErrRoomNotFound)
	if err != nil {
		return Room{}, err
	}
	clusterPath := filepath.Join(roomPath, "cluster.ini")
	config, err := ini.Load(clusterPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Room{}, ErrInvalidRoom
		}
		return Room{}, fmt.Errorf("parse %s: %w", clusterPath, err)
	}
	info, err := os.Stat(clusterPath)
	if err != nil {
		return Room{}, fmt.Errorf("inspect %s: %w", clusterPath, err)
	}
	roomID := EncodeID(directoryName)
	managed := false
	if c.managed != nil {
		managed, err = c.managed.IsManaged(roomID)
		if err != nil {
			return Room{}, fmt.Errorf("inspect managed room: %w", err)
		}
	}
	worlds, err := c.Worlds(roomID)
	if err != nil {
		return Room{}, err
	}
	name := strings.TrimSpace(config.Section("NETWORK").Key("cluster_name").String())
	if name == "" {
		name = directoryName
	}
	return Room{
		ID:                roomID,
		DirectoryName:     directoryName,
		Name:              name,
		Description:       strings.TrimSpace(config.Section("NETWORK").Key("cluster_description").String()),
		GameMode:          config.Section("GAMEPLAY").Key("game_mode").MustString("survival"),
		MaxPlayers:        config.Section("GAMEPLAY").Key("max_players").MustInt(6),
		PvP:               config.Section("GAMEPLAY").Key("pvp").MustBool(false),
		PasswordProtected: config.Section("NETWORK").Key("cluster_password").String() != "",
		Managed:           managed,
		WorldCount:        len(worlds),
		UpdatedAt:         info.ModTime(),
	}, nil
}

func (c *Catalog) worldFromName(roomID, roomPath, directoryName string) (World, error) {
	worldPath, err := c.existingDirectory(roomPath, directoryName, ErrWorldNotFound)
	if err != nil {
		return World{}, err
	}
	serverPath := filepath.Join(worldPath, "server.ini")
	config, err := ini.Load(serverPath)
	if err != nil {
		if os.IsNotExist(err) {
			return World{}, ErrInvalidWorld
		}
		return World{}, fmt.Errorf("parse %s: %w", serverPath, err)
	}
	info, err := os.Stat(serverPath)
	if err != nil {
		return World{}, fmt.Errorf("inspect %s: %w", serverPath, err)
	}
	isMaster := config.Section("SHARD").Key("is_master").MustBool(false)
	role := WorldRoleCustom
	if isMaster {
		role = WorldRoleMaster
	} else if strings.Contains(strings.ToLower(directoryName), "cave") {
		role = WorldRoleCaves
	}
	return World{
		ID:            EncodeID(directoryName),
		RoomID:        roomID,
		DirectoryName: directoryName,
		Name:          directoryName,
		Role:          role,
		IsMaster:      isMaster,
		ServerPort:    config.Section("NETWORK").Key("server_port").MustInt(0),
		UpdatedAt:     info.ModTime(),
	}, nil
}

func (c *Catalog) existingDirectory(parent, name string, notFound error) (string, error) {
	if err := validateComponent(name); err != nil {
		return "", err
	}
	path := filepath.Join(parent, name)
	if err := ensureContained(parent, path); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", notFound
		}
		return "", fmt.Errorf("inspect directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", ErrUnsafePath
	}
	return path, nil
}

func EncodeID(name string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(name))
}

func DecodeID(id string) (string, error) {
	if id == "" {
		return "", ErrInvalidID
	}
	decoded, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || EncodeID(string(decoded)) != id {
		return "", ErrInvalidID
	}
	name := string(decoded)
	if err := validateComponent(name); err != nil {
		return "", ErrInvalidID
	}
	return name, nil
}

func validateComponent(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00\r\n") {
		return ErrUnsafePath
	}
	return nil
}

func ensureContained(root, candidate string) error {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.IsAbs(relative) {
		return ErrUnsafePath
	}
	return nil
}

func worldRoleOrder(role WorldRole) int {
	switch role {
	case WorldRoleMaster:
		return 0
	case WorldRoleCaves:
		return 1
	default:
		return 2
	}
}
