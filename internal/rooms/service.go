package rooms

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dont/internal/roomops"
	worldtemplate "dont/template"

	"github.com/go-ini/ini"
)

var directoryNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
var recoveryNamePattern = regexp.MustCompile(`^([0-9]{10,20})-([A-Za-z0-9][A-Za-z0-9_-]{0,63})$`)

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

type DeleteRoomRequest struct {
	Confirmation string `json:"confirmation"`
}

type DeleteRoomResult struct {
	Room         Room   `json:"room"`
	RecoveryName string `json:"recoveryName"`
}

type DeleteWorldResult struct {
	World        World  `json:"world"`
	RecoveryName string `json:"recoveryName"`
}

type RecoveryItem struct {
	RecoveryName  string    `json:"recoveryName"`
	DirectoryName string    `json:"directoryName"`
	DisplayName   string    `json:"displayName"`
	DeletedAt     time.Time `json:"deletedAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type PurgeRecoveryRequest struct {
	Confirmation string `json:"confirmation"`
}

type Service struct {
	catalog         *Catalog
	store           *Store
	localDiscovery  bool
	worldMu         sync.Mutex
	lifecycleMu     sync.RWMutex
	onManaged       []func(string)
	onUnmanaged     []func(string)
	onWorld         []func(string, string)
	initializeWorld func(string, string) error
}

func NewService(catalog *Catalog, store *Store) *Service {
	return &Service{catalog: catalog, store: store, localDiscovery: true}
}

// ConfigureWorldInitializer prepares managed assets in newly created staging
// directories, before the room/world is published. It never runs on discovery,
// adoption, restoration, or ordinary starts.
func (s *Service) ConfigureWorldInitializer(initialize func(root, world string) error) {
	s.initializeWorld = initialize
}

func (s *Service) initializeNewWorld(root, name string) error {
	if s.initializeWorld == nil {
		return nil
	}
	if err := s.initializeWorld(root, name); err != nil {
		return fmt.Errorf("initialize %s world runtime: %w", name, err)
	}
	return nil
}

// ConfigureLocalDiscovery controls whether the controller's save path is a
// runtime source. Controller-only deployments keep the local file catalog out
// of the fleet while still retaining discovered Agent rooms in the database.
func (s *Service) ConfigureLocalDiscovery(enabled bool) {
	s.localDiscovery = enabled
}

func (s *Service) localRoomValue(room Room) Room {
	room.TargetIDs, room.AvailableTargetIDs = []string{}, []string{}
	if s.localDiscovery {
		room.TargetIDs, room.AvailableTargetIDs = []string{"local"}, []string{"local"}
	}
	return decorateRoomControl(room)
}

func (s *Service) localWorldValue(world World) World {
	world.TargetIDs, world.AvailableTargetIDs = []string{}, []string{}
	if s.localDiscovery {
		world.TargetIDs, world.AvailableTargetIDs = []string{"local"}, []string{"local"}
	}
	return world
}

func (s *Service) persistLocalCatalogRoom(roomID string) error {
	room, err := s.catalog.Room(roomID)
	if err != nil {
		return err
	}
	worlds, err := s.catalog.Worlds(roomID)
	if err != nil {
		return err
	}
	room = s.localRoomValue(room)
	quality := make(map[string]int, len(worlds))
	for index := range worlds {
		worlds[index] = s.localWorldValue(worlds[index])
		quality[worlds[index].ID] = 100
	}
	room.WorldCount = len(worlds)
	return s.store.SaveRuntimeCatalogRoom(catalogRoomValue{
		Room: room, Worlds: worlds, MetadataQuality: 100, WorldQuality: quality,
	})
}

func (s *Service) SetManagedRoomLifecycle(onManaged, onUnmanaged func(string)) {
	s.lifecycleMu.Lock()
	s.onManaged = callbacks(onManaged)
	s.onUnmanaged = callbacks(onUnmanaged)
	s.lifecycleMu.Unlock()
}

func (s *Service) AddManagedRoomLifecycle(onManaged, onUnmanaged func(string)) {
	s.lifecycleMu.Lock()
	if onManaged != nil {
		s.onManaged = append(s.onManaged, onManaged)
	}
	if onUnmanaged != nil {
		s.onUnmanaged = append(s.onUnmanaged, onUnmanaged)
	}
	s.lifecycleMu.Unlock()
}

func (s *Service) AddWorldLifecycle(onCreated func(string, string)) {
	if onCreated == nil {
		return
	}
	s.lifecycleMu.Lock()
	s.onWorld = append(s.onWorld, onCreated)
	s.lifecycleMu.Unlock()
}

func (s *Service) SyncRuntimeCatalog(sources []RuntimeCatalogSource) error {
	local := []sourceRoomValue{}
	if s.localDiscovery {
		for _, source := range sources {
			if source.TargetID != "local" || !source.Available {
				continue
			}
			items, err := localSourceRooms(s.catalog)
			if err != nil {
				return err
			}
			local = items
			break
		}
	}
	values := mergeRuntimeSources(sources, local)
	confirmedTargets := make(map[string]bool, len(sources))
	for _, source := range sources {
		if source.Online && source.Available && !source.Stale {
			confirmedTargets[source.TargetID] = true
		}
	}
	newlyRegistered := make([]string, 0, len(values))
	for _, value := range values {
		registered, err := s.store.IsManaged(value.Room.ID)
		if err != nil {
			return err
		}
		if !registered {
			newlyRegistered = append(newlyRegistered, value.Room.ID)
		}
	}
	removedRooms, err := s.store.ReplaceRuntimeCatalog(values, confirmedTargets)
	if err != nil {
		return err
	}
	for _, value := range values {
		if err := s.store.Adopt(value.Room); err != nil {
			return err
		}
	}
	for _, roomID := range newlyRegistered {
		s.notifyManagedRoom(roomID, true)
	}
	for _, roomID := range removedRooms {
		s.notifyManagedRoom(roomID, false)
	}
	return nil
}

func (s *Service) List() ([]Room, error) {
	persisted, err := s.store.CatalogRooms()
	if err != nil {
		return nil, catalogLookupError(err, ErrRoomNotFound)
	}
	byID := make(map[string]Room, len(persisted))
	for _, room := range persisted {
		byID[room.ID] = room
	}
	if s.localDiscovery {
		local, localErr := s.catalog.List()
		if localErr != nil {
			return nil, localErr
		}
		for _, room := range local {
			room.TargetIDs, room.AvailableTargetIDs = []string{"local"}, []string{"local"}
			if stored, exists := byID[room.ID]; exists {
				room = mergeRoomValues(room, stored)
			}
			byID[room.ID] = room
		}
	}
	items := make([]Room, 0, len(byID))
	for _, room := range byID {
		items = append(items, decorateRoomControl(room))
	}
	sort.Slice(items, func(i, j int) bool {
		if !strings.EqualFold(items[i].Name, items[j].Name) {
			return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
		}
		return items[i].ID < items[j].ID
	})
	return items, nil
}

func (s *Service) Room(roomID string) (Room, error) {
	if _, err := DecodeID(roomID); err != nil {
		return Room{}, err
	}
	var local Room
	localFound := false
	if s.localDiscovery {
		value, err := s.catalog.Room(roomID)
		if err == nil {
			local, localFound = value, true
			local.TargetIDs, local.AvailableTargetIDs = []string{"local"}, []string{"local"}
		} else if !errors.Is(err, ErrRoomNotFound) {
			return Room{}, err
		}
	}
	persisted, persistedErr := s.store.CatalogRoom(roomID)
	persistedFound := persistedErr == nil
	if persistedErr != nil && !errors.Is(persistedErr, ErrRoomNotFound) {
		return Room{}, catalogLookupError(persistedErr, ErrRoomNotFound)
	}
	switch {
	case localFound && persistedFound:
		return decorateRoomControl(mergeRoomValues(local, persisted)), nil
	case localFound:
		return decorateRoomControl(local), nil
	case persistedFound:
		return decorateRoomControl(persisted), nil
	default:
		return Room{}, ErrRoomNotFound
	}
}

func decorateRoomControl(room Room) Room {
	room.Managed = true
	targets := mergeIDs(room.TargetIDs)
	available := mergeIDs(room.AvailableTargetIDs)
	room.TargetIDs, room.AvailableTargetIDs = targets, available
	switch {
	case len(targets) == 0:
		room.ControlState = "unavailable"
	case len(available) == 0:
		room.ControlState = "offline"
	case len(available) < len(targets):
		room.ControlState = "degraded"
	default:
		room.ControlState = "ready"
	}
	room.ControlAvailable = room.ControlState == "ready"
	return room
}

func (s *Service) Worlds(roomID string) ([]World, error) {
	if _, err := DecodeID(roomID); err != nil {
		return nil, err
	}
	if _, err := s.Room(roomID); err != nil {
		return nil, err
	}
	persisted, err := s.store.CatalogWorlds(roomID)
	if err != nil {
		return nil, catalogLookupError(err, ErrWorldNotFound)
	}
	byID := make(map[string]World, len(persisted))
	for _, world := range persisted {
		byID[world.ID] = world
	}
	if s.localDiscovery {
		local, localErr := s.catalog.Worlds(roomID)
		if localErr == nil {
			for _, world := range local {
				world.TargetIDs, world.AvailableTargetIDs = []string{"local"}, []string{"local"}
				if stored, exists := byID[world.ID]; exists {
					world = mergeWorldValues(world, stored)
				}
				byID[world.ID] = world
			}
		} else if !errors.Is(localErr, ErrRoomNotFound) {
			return nil, localErr
		}
	}
	items := make([]World, 0, len(byID))
	for _, world := range byID {
		items = append(items, world)
	}
	sortWorlds(items)
	return items, nil
}

func (s *Service) World(roomID, worldID string) (World, error) {
	if _, err := DecodeID(worldID); err != nil {
		return World{}, err
	}
	worlds, err := s.Worlds(roomID)
	if err != nil {
		return World{}, err
	}
	for _, world := range worlds {
		if world.ID == worldID {
			return world, nil
		}
	}
	return World{}, ErrWorldNotFound
}

func (s *Service) Adopt(roomID string) (Room, error) {
	return s.Register(roomID)
}

// Register persists a discovered room in the controller catalog. Discovery
// calls this automatically; the public Adopt method remains as a compatibility
// alias for older clients.
func (s *Service) Register(roomID string) (Room, error) {
	room, err := s.Room(roomID)
	if err != nil {
		return Room{}, err
	}
	registered, err := s.store.IsManaged(room.ID)
	if err != nil {
		return Room{}, err
	}
	if err := s.store.Adopt(room); err != nil {
		return Room{}, err
	}
	room = decorateRoomControl(room)
	if !registered {
		s.notifyManagedRoom(room.ID, true)
	}
	return room, nil
}

func (s *Service) Unadopt(roomID string) error {
	return s.Unregister(roomID)
}

func (s *Service) Unregister(roomID string) error {
	if err := s.store.Unadopt(roomID); err != nil {
		return err
	}
	s.notifyManagedRoom(roomID, false)
	return nil
}

func (s *Service) Create(request CreateRequest) (Room, error) {
	request.DirectoryName = strings.TrimSpace(request.DirectoryName)
	request.Name = strings.TrimSpace(request.Name)
	request.Description = strings.TrimSpace(request.Description)
	request.GameMode = strings.TrimSpace(strings.ToLower(request.GameMode))
	if err := validateCreateRequest(request); err != nil {
		return Room{}, err
	}
	_, release, err := roomops.Acquire(context.Background(), EncodeID(request.DirectoryName))
	if err != nil {
		return Room{}, err
	}
	defer release()
	s.worldMu.Lock()
	defer s.worldMu.Unlock()
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
	allocation, err := s.nextRoomAllocation(request.IncludeCaves)
	if err != nil {
		return Room{}, err
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
	if err := writeRoomFiles(temporary, request, allocation); err != nil {
		return Room{}, err
	}
	if err := s.initializeNewWorld(temporary, "Master"); err != nil {
		return Room{}, err
	}
	if request.IncludeCaves {
		if err := s.initializeNewWorld(temporary, "Caves"); err != nil {
			return Room{}, err
		}
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
	if err := s.persistLocalCatalogRoom(room.ID); err != nil {
		_ = s.store.Unadopt(room.ID)
		return Room{}, err
	}
	room.Managed = true
	room = s.localRoomValue(room)
	completed = true
	s.notifyManagedRoom(room.ID, true)
	return room, nil
}

func (s *Service) CreateWorld(roomID string, request CreateWorldRequest) (World, error) {
	_, release, err := roomops.Acquire(context.Background(), roomID)
	if err != nil {
		return World{}, err
	}
	defer release()
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
	if err := s.initializeNewWorld(temporary, request.DirectoryName); err != nil {
		return World{}, err
	}
	if err := os.Rename(filepath.Join(temporary, request.DirectoryName), target); err != nil {
		return World{}, fmt.Errorf("publish world directory: %w", err)
	}
	published := true
	defer func() {
		if published {
			_ = os.RemoveAll(target)
		}
	}()
	world, err := s.catalog.World(roomID, EncodeID(request.DirectoryName))
	if err != nil {
		return World{}, err
	}
	if err := s.persistLocalCatalogRoom(room.ID); err != nil {
		return World{}, err
	}
	published = false
	world = s.localWorldValue(world)
	s.notifyWorldCreated(room.ID, world.ID)
	return world, nil
}

func (s *Service) DeleteRoom(roomID string, request DeleteRoomRequest) (DeleteRoomResult, error) {
	_, release, err := roomops.Acquire(context.Background(), roomID)
	if err != nil {
		return DeleteRoomResult{}, err
	}
	defer release()
	s.worldMu.Lock()
	defer s.worldMu.Unlock()

	room, err := s.catalog.Room(roomID)
	if err != nil {
		return DeleteRoomResult{}, err
	}
	if !room.Managed {
		return DeleteRoomResult{}, ErrRoomNotManaged
	}
	if request.Confirmation != room.Name {
		return DeleteRoomResult{}, ErrConfirmation
	}
	source := filepath.Join(s.catalog.root, room.DirectoryName)
	trashRoot := filepath.Join(s.catalog.root, ".dst-admin-trash")
	if err := ensureContained(s.catalog.root, source); err != nil {
		return DeleteRoomResult{}, err
	}
	if err := os.MkdirAll(trashRoot, 0750); err != nil {
		return DeleteRoomResult{}, fmt.Errorf("create room recovery directory: %w", err)
	}
	trashName := strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + room.DirectoryName
	target := filepath.Join(trashRoot, trashName)
	if err := os.Rename(source, target); err != nil {
		return DeleteRoomResult{}, fmt.Errorf("move room to recovery directory: %w", err)
	}
	if err := s.store.Unadopt(room.ID); err != nil {
		if rollbackErr := os.Rename(target, source); rollbackErr != nil {
			return DeleteRoomResult{}, fmt.Errorf("%w; restore room directory: %v", err, rollbackErr)
		}
		return DeleteRoomResult{}, err
	}
	if err := s.store.RemoveRuntimeCatalogRoom(room.ID); err != nil {
		if rollbackErr := os.Rename(target, source); rollbackErr != nil {
			return DeleteRoomResult{}, fmt.Errorf("%w; restore room directory: %v", err, rollbackErr)
		}
		_ = s.store.Adopt(room)
		return DeleteRoomResult{}, err
	}
	s.notifyManagedRoom(room.ID, false)
	room = s.localRoomValue(room)
	return DeleteRoomResult{
		Room:         room,
		RecoveryName: filepath.Join(".dst-admin-trash", trashName),
	}, nil
}

func (s *Service) HasLocalRoom(roomID string) (bool, error) {
	if _, err := s.catalog.Room(roomID); err != nil {
		if errors.Is(err, ErrRoomNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// FinalizeRemoteRoomRecovery removes control-plane registration only after a
// Runtime has confirmed that its room directory was moved to recovery.
func (s *Service) FinalizeRemoteRoomRecovery(roomID string) (Room, error) {
	s.worldMu.Lock()
	defer s.worldMu.Unlock()
	room, err := s.Room(roomID)
	if err != nil {
		return Room{}, err
	}
	if !room.Managed {
		return Room{}, ErrRoomNotManaged
	}
	if err := s.store.Unadopt(room.ID); err != nil {
		return Room{}, err
	}
	if err := s.store.RemoveRuntimeCatalogRoom(room.ID); err != nil {
		_ = s.store.Adopt(room)
		return Room{}, err
	}
	s.notifyManagedRoom(room.ID, false)
	return room, nil
}

func (s *Service) notifyManagedRoom(roomID string, managed bool) {
	s.lifecycleMu.RLock()
	notify := append([]func(string){}, s.onUnmanaged...)
	if managed {
		notify = append([]func(string){}, s.onManaged...)
	}
	s.lifecycleMu.RUnlock()
	for _, callback := range notify {
		callback(roomID)
	}
}

func (s *Service) notifyWorldCreated(roomID, worldID string) {
	s.lifecycleMu.RLock()
	notify := append([]func(string, string){}, s.onWorld...)
	s.lifecycleMu.RUnlock()
	for _, callback := range notify {
		callback(roomID, worldID)
	}
}

func callbacks(callback func(string)) []func(string) {
	if callback == nil {
		return nil
	}
	return []func(string){callback}
}

func (s *Service) DeleteWorld(roomID, worldID string, request DeleteWorldRequest) (DeleteWorldResult, error) {
	_, release, err := roomops.Acquire(context.Background(), roomID)
	if err != nil {
		return DeleteWorldResult{}, err
	}
	defer release()
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
	world = s.localWorldValue(world)
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
	if err := s.persistLocalCatalogRoom(room.ID); err != nil {
		if rollbackErr := os.Rename(target, source); rollbackErr != nil {
			return DeleteWorldResult{}, fmt.Errorf("%w; restore world directory: %v", err, rollbackErr)
		}
		return DeleteWorldResult{}, err
	}
	return DeleteWorldResult{World: world, RecoveryName: filepath.Join(".dst-admin-trash", trashName)}, nil
}

func (s *Service) ListRoomRecoveries() ([]RecoveryItem, error) {
	return listRecoveries(filepath.Join(s.catalog.root, ".dst-admin-trash"), "cluster.ini", "NETWORK", "cluster_name")
}

func (s *Service) RestoreRoom(recoveryName string) (Room, error) {
	directoryName, _, err := parseRecoveryName(recoveryName)
	if err != nil {
		return Room{}, err
	}
	roomID := EncodeID(directoryName)
	_, release, err := roomops.Acquire(context.Background(), roomID)
	if err != nil {
		return Room{}, err
	}
	defer release()
	s.worldMu.Lock()
	defer s.worldMu.Unlock()

	trashRoot := filepath.Join(s.catalog.root, ".dst-admin-trash")
	source, err := recoveryDirectory(trashRoot, recoveryName)
	if err != nil {
		return Room{}, err
	}
	target := filepath.Join(s.catalog.root, directoryName)
	if err := ensureContained(s.catalog.root, target); err != nil {
		return Room{}, err
	}
	if _, err := os.Lstat(target); err == nil {
		return Room{}, ErrRoomExists
	} else if !os.IsNotExist(err) {
		return Room{}, fmt.Errorf("inspect room restore target: %w", err)
	}
	if err := os.Rename(source, target); err != nil {
		return Room{}, fmt.Errorf("restore room directory: %w", err)
	}
	room, err := s.catalog.Room(roomID)
	if err != nil {
		_ = os.Rename(target, source)
		return Room{}, err
	}
	if err := s.store.Adopt(room); err != nil {
		if rollbackErr := os.Rename(target, source); rollbackErr != nil {
			return Room{}, fmt.Errorf("%w; return room to recovery directory: %v", err, rollbackErr)
		}
		return Room{}, err
	}
	if err := s.persistLocalCatalogRoom(room.ID); err != nil {
		_ = s.store.Unadopt(room.ID)
		_ = os.Rename(target, source)
		return Room{}, err
	}
	room.Managed = true
	room = s.localRoomValue(room)
	s.notifyManagedRoom(room.ID, true)
	return room, nil
}

func (s *Service) PurgeRoomRecovery(recoveryName string, request PurgeRecoveryRequest) error {
	if request.Confirmation != recoveryName {
		return ErrRecoveryConfirmation
	}
	directoryName, _, err := parseRecoveryName(recoveryName)
	if err != nil {
		return err
	}
	_, release, err := roomops.Acquire(context.Background(), EncodeID(directoryName))
	if err != nil {
		return err
	}
	defer release()
	s.worldMu.Lock()
	defer s.worldMu.Unlock()
	source, err := recoveryDirectory(filepath.Join(s.catalog.root, ".dst-admin-trash"), recoveryName)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(source); err != nil {
		return fmt.Errorf("purge room recovery: %w", err)
	}
	return nil
}

func (s *Service) ListWorldRecoveries(roomID string) ([]RecoveryItem, error) {
	room, err := s.catalog.Room(roomID)
	if err != nil {
		return nil, err
	}
	roomPath := filepath.Join(s.catalog.root, room.DirectoryName)
	return listRecoveries(filepath.Join(roomPath, ".dst-admin-trash"), "server.ini", "SHARD", "name")
}

func (s *Service) RestoreWorld(roomID, recoveryName string) (World, error) {
	directoryName, _, err := parseRecoveryName(recoveryName)
	if err != nil {
		return World{}, err
	}
	_, release, err := roomops.Acquire(context.Background(), roomID)
	if err != nil {
		return World{}, err
	}
	defer release()
	s.worldMu.Lock()
	defer s.worldMu.Unlock()

	room, err := s.catalog.Room(roomID)
	if err != nil {
		return World{}, err
	}
	if !room.Managed {
		return World{}, ErrRoomNotManaged
	}
	roomPath := filepath.Join(s.catalog.root, room.DirectoryName)
	source, err := recoveryDirectory(filepath.Join(roomPath, ".dst-admin-trash"), recoveryName)
	if err != nil {
		return World{}, err
	}
	target := filepath.Join(roomPath, directoryName)
	if err := ensureContained(roomPath, target); err != nil {
		return World{}, err
	}
	if _, err := os.Lstat(target); err == nil {
		return World{}, ErrWorldExists
	} else if !os.IsNotExist(err) {
		return World{}, fmt.Errorf("inspect world restore target: %w", err)
	}
	if err := os.Rename(source, target); err != nil {
		return World{}, fmt.Errorf("restore world directory: %w", err)
	}
	world, err := s.catalog.World(room.ID, EncodeID(directoryName))
	if err != nil {
		_ = os.Rename(target, source)
		return World{}, err
	}
	if err := s.persistLocalCatalogRoom(room.ID); err != nil {
		_ = os.Rename(target, source)
		return World{}, err
	}
	world = s.localWorldValue(world)
	s.notifyWorldCreated(room.ID, world.ID)
	return world, nil
}

func (s *Service) PurgeWorldRecovery(roomID, recoveryName string, request PurgeRecoveryRequest) error {
	if request.Confirmation != recoveryName {
		return ErrRecoveryConfirmation
	}
	if _, _, err := parseRecoveryName(recoveryName); err != nil {
		return err
	}
	_, release, err := roomops.Acquire(context.Background(), roomID)
	if err != nil {
		return err
	}
	defer release()
	s.worldMu.Lock()
	defer s.worldMu.Unlock()
	room, err := s.catalog.Room(roomID)
	if err != nil {
		return err
	}
	roomPath := filepath.Join(s.catalog.root, room.DirectoryName)
	source, err := recoveryDirectory(filepath.Join(roomPath, ".dst-admin-trash"), recoveryName)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(source); err != nil {
		return fmt.Errorf("purge world recovery: %w", err)
	}
	return nil
}

func listRecoveries(trashRoot, metadataFile, section, key string) ([]RecoveryItem, error) {
	entries, err := os.ReadDir(trashRoot)
	if os.IsNotExist(err) {
		return []RecoveryItem{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read recovery directory: %w", err)
	}
	items := make([]RecoveryItem, 0, len(entries))
	for _, entry := range entries {
		directoryName, deletedAt, parseErr := parseRecoveryName(entry.Name())
		if parseErr != nil || entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		path, pathErr := recoveryDirectory(trashRoot, entry.Name())
		if pathErr != nil {
			continue
		}
		info, statErr := os.Stat(filepath.Join(path, metadataFile))
		if statErr != nil || !info.Mode().IsRegular() {
			continue
		}
		displayName := directoryName
		if config, loadErr := ini.Load(filepath.Join(path, metadataFile)); loadErr == nil {
			if value := strings.TrimSpace(config.Section(section).Key(key).String()); value != "" {
				displayName = value
			}
		}
		items = append(items, RecoveryItem{
			RecoveryName: entry.Name(), DirectoryName: directoryName, DisplayName: displayName,
			DeletedAt: deletedAt, UpdatedAt: info.ModTime(),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].DeletedAt.After(items[j].DeletedAt) })
	return items, nil
}

func parseRecoveryName(name string) (string, time.Time, error) {
	if err := validateComponent(name); err != nil {
		return "", time.Time{}, err
	}
	matches := recoveryNamePattern.FindStringSubmatch(name)
	if len(matches) != 3 || !directoryNamePattern.MatchString(matches[2]) {
		return "", time.Time{}, ErrUnsafePath
	}
	timestamp, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil || timestamp <= 0 {
		return "", time.Time{}, ErrUnsafePath
	}
	return matches[2], time.Unix(0, timestamp).UTC(), nil
}

func recoveryDirectory(trashRoot, recoveryName string) (string, error) {
	if _, _, err := parseRecoveryName(recoveryName); err != nil {
		return "", err
	}
	path := filepath.Join(trashRoot, recoveryName)
	if err := ensureContained(trashRoot, path); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", ErrRecoveryNotFound
	}
	if err != nil {
		return "", fmt.Errorf("inspect recovery directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", ErrUnsafePath
	}
	return path, nil
}

type worldAllocation struct {
	shardID            int
	serverPort         int
	authenticationPort int
	masterServerPort   int
	master             bool
}

type roomAllocation struct {
	masterPort        int
	masterWorld       worldAllocation
	cavesWorld        worldAllocation
	includeCavesWorld bool
}

func (s *Service) nextRoomAllocation(includeCaves bool) (roomAllocation, error) {
	usedPorts, err := s.configuredPorts()
	if err != nil {
		return roomAllocation{}, err
	}
	reserve := func(preferred int) (int, error) {
		port := nextFreePort(preferred, usedPorts)
		if port < 1 || port > 65535 {
			return 0, errors.New("没有可用于新房间的 UDP 端口")
		}
		usedPorts[port] = true
		return port, nil
	}
	masterPort, err := reserve(10889)
	if err != nil {
		return roomAllocation{}, err
	}
	masterServer, err := reserve(10999)
	if err != nil {
		return roomAllocation{}, err
	}
	masterAuth, err := reserve(8767)
	if err != nil {
		return roomAllocation{}, err
	}
	masterSteam, err := reserve(27017)
	if err != nil {
		return roomAllocation{}, err
	}
	result := roomAllocation{
		masterPort: masterPort,
		masterWorld: worldAllocation{
			shardID: 1, serverPort: masterServer, authenticationPort: masterAuth,
			masterServerPort: masterSteam, master: true,
		},
		includeCavesWorld: includeCaves,
	}
	if !includeCaves {
		return result, nil
	}
	cavesServer, err := reserve(11000)
	if err != nil {
		return roomAllocation{}, err
	}
	cavesAuth, err := reserve(8768)
	if err != nil {
		return roomAllocation{}, err
	}
	cavesSteam, err := reserve(27018)
	if err != nil {
		return roomAllocation{}, err
	}
	result.cavesWorld = worldAllocation{
		shardID: 2, serverPort: cavesServer, authenticationPort: cavesAuth,
		masterServerPort: cavesSteam, master: false,
	}
	return result, nil
}

func (s *Service) nextWorldAllocation(roomID, roomPath, worldType string) (worldAllocation, error) {
	worlds, err := s.catalog.Worlds(roomID)
	if err != nil {
		return worldAllocation{}, err
	}
	usedShardIDs := make(map[int]bool)
	usedPorts, err := s.configuredPorts()
	if err != nil {
		return worldAllocation{}, err
	}
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
		hasMaster = hasMaster || world.IsMaster
	}
	master := !hasMaster
	shardID := 2
	if master {
		shardID = 1
	}
	for usedShardIDs[shardID] {
		shardID++
	}
	reserve := func(preferred int) (int, error) {
		port := nextFreePort(preferred, usedPorts)
		if port < 1 || port > 65535 {
			return 0, errors.New("没有可用于新世界的 UDP 端口")
		}
		usedPorts[port] = true
		return port, nil
	}
	serverPort, err := reserve(10998 + shardID)
	if err != nil {
		return worldAllocation{}, err
	}
	authenticationPort, err := reserve(8766 + shardID)
	if err != nil {
		return worldAllocation{}, err
	}
	masterServerPort, err := reserve(27016 + shardID)
	if err != nil {
		return worldAllocation{}, err
	}
	return worldAllocation{
		shardID: shardID, serverPort: serverPort, authenticationPort: authenticationPort,
		masterServerPort: masterServerPort, master: master,
	}, nil
}

func (s *Service) configuredPorts() (map[int]bool, error) {
	result := make(map[int]bool)
	rooms, err := s.catalog.List()
	if err != nil {
		return nil, err
	}
	for _, room := range rooms {
		roomPath := filepath.Join(s.catalog.root, room.DirectoryName)
		cluster, loadErr := ini.Load(filepath.Join(roomPath, "cluster.ini"))
		if loadErr != nil {
			return nil, fmt.Errorf("parse existing room cluster.ini: %w", loadErr)
		}
		if port := cluster.Section("SHARD").Key("master_port").MustInt(0); port > 0 {
			result[port] = true
		}
		worlds, worldsErr := s.catalog.Worlds(room.ID)
		if worldsErr != nil {
			return nil, worldsErr
		}
		for _, world := range worlds {
			config, configErr := ini.Load(filepath.Join(roomPath, world.DirectoryName, "server.ini"))
			if configErr != nil {
				return nil, fmt.Errorf("parse existing world server.ini: %w", configErr)
			}
			for _, port := range []int{
				config.Section("NETWORK").Key("server_port").MustInt(0),
				config.Section("STEAM").Key("authentication_port").MustInt(0),
				config.Section("STEAM").Key("master_server_port").MustInt(0),
			} {
				if port > 0 {
					result[port] = true
				}
			}
		}
	}
	return result, nil
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
	if err := ValidateClusterToken(request.ClusterToken, true); err != nil {
		details["clusterToken"] = err.Error()
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

func writeRoomFiles(root string, request CreateRequest, allocation roomAllocation) error {
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
	_, _ = network.NewKey("cluster_language", "zh")
	misc, _ := cluster.NewSection("MISC")
	_, _ = misc.NewKey("console_enabled", "true")
	shard, _ := cluster.NewSection("SHARD")
	_, _ = shard.NewKey("shard_enabled", "true")
	_, _ = shard.NewKey("bind_ip", "127.0.0.1")
	_, _ = shard.NewKey("master_ip", "127.0.0.1")
	_, _ = shard.NewKey("master_port", strconv.Itoa(allocation.masterPort))
	_, _ = shard.NewKey("cluster_key", clusterKey)
	if err := writeINI(filepath.Join(root, "cluster.ini"), cluster, 0640); err != nil {
		return err
	}
	if token := strings.TrimSpace(request.ClusterToken); token != "" {
		if err := os.WriteFile(filepath.Join(root, "cluster_token.txt"), []byte(token+"\n"), 0600); err != nil {
			return fmt.Errorf("write cluster token: %w", err)
		}
	}
	if err := writeWorld(
		root, "Master", allocation.masterWorld.shardID, allocation.masterWorld.serverPort,
		allocation.masterWorld.authenticationPort, allocation.masterWorld.masterServerPort, true, "forest",
	); err != nil {
		return err
	}
	if allocation.includeCavesWorld {
		if err := writeWorld(
			root, "Caves", allocation.cavesWorld.shardID, allocation.cavesWorld.serverPort,
			allocation.cavesWorld.authenticationPort, allocation.cavesWorld.masterServerPort, false, "cave",
		); err != nil {
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
	override, err := worldtemplate.LevelDataOverride(location)
	if err != nil {
		return fmt.Errorf("compose %s world override: %w", name, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "leveldataoverride.lua"), override, 0640); err != nil {
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
