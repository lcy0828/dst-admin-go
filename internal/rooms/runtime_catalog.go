package rooms

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"dont/internal/worldidentity"
	"dont/shared"

	"github.com/jinzhu/gorm"
)

// RuntimeCatalogSource is one target's normalized inventory. Paths remain on
// the target and are deliberately not persisted in the logical room catalog.
type RuntimeCatalogSource struct {
	TargetID   string
	Online     bool
	Available  bool
	Stale      bool
	ObservedAt time.Time
	Inventory  shared.RuntimeInventoryReport
}

type catalogRoomRecord struct {
	RoomID             string `gorm:"type:varchar(255);primary_key"`
	DirectoryName      string `gorm:"type:varchar(255);not null;unique_index"`
	Name               string `gorm:"type:varchar(255);not null"`
	Description        string `gorm:"type:text;not null"`
	GameMode           string `gorm:"type:varchar(64);not null"`
	MaxPlayers         int    `gorm:"not null"`
	PvP                bool   `gorm:"column:pvp"`
	PasswordProtected  bool
	TargetIDs          string `gorm:"type:text;not null"`
	AvailableTargetIDs string `gorm:"type:text;not null"`
	MetadataQuality    int    `gorm:"not null;default:0"`
	ObservedAt         time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type catalogWorldRecord struct {
	CatalogID          string `gorm:"type:char(64);primary_key"`
	RoomID             string `gorm:"type:varchar(255);not null;index"`
	WorldID            string `gorm:"type:varchar(255);not null;index"`
	DirectoryName      string `gorm:"type:varchar(255);not null"`
	Name               string `gorm:"type:varchar(255);not null"`
	Role               string `gorm:"type:varchar(24);not null"`
	Type               string `gorm:"type:varchar(24);not null"`
	IsMaster           bool
	ShardID            int
	ServerPort         int
	TargetIDs          string `gorm:"type:text;not null"`
	AvailableTargetIDs string `gorm:"type:text;not null"`
	MetadataQuality    int    `gorm:"not null;default:0"`
	ObservedAt         time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type catalogRoomValue struct {
	Room            Room
	Worlds          []World
	MetadataQuality int
	WorldQuality    map[string]int
}

type sourceRoomValue struct {
	room    Room
	worlds  []World
	quality int
}

func catalogWorldID(roomID, worldID string) string {
	digest := sha256.Sum256([]byte(roomID + "\x00" + worldID))
	return hex.EncodeToString(digest[:])
}

func runtimeSourceRooms(source RuntimeCatalogSource) []sourceRoomValue {
	if !source.Available {
		return nil
	}
	observedAt := source.ObservedAt.UTC()
	if observedAt.IsZero() {
		observedAt = source.Inventory.ObservedAt.UTC()
	}
	values := make([]sourceRoomValue, 0, len(source.Inventory.Rooms))
	for _, reportedRoom := range source.Inventory.Rooms {
		directory := strings.TrimSpace(reportedRoom.Directory)
		if validateComponent(directory) != nil || len(directory) > 255 {
			continue
		}
		roomID := EncodeID(directory)
		name := strings.TrimSpace(reportedRoom.Name)
		if name == "" {
			name = directory
		}
		room := Room{
			ID: roomID, DirectoryName: directory, Name: name, GameMode: "survival", MaxPlayers: 6,
			Managed: true, TargetIDs: []string{source.TargetID}, UpdatedAt: observedAt,
		}
		if source.Online && !source.Stale {
			room.AvailableTargetIDs = []string{source.TargetID}
		}
		worlds := make([]World, 0, len(reportedRoom.Shards))
		for _, reportedWorld := range reportedRoom.Shards {
			worldDirectory := strings.TrimSpace(reportedWorld.Directory)
			if validateComponent(worldDirectory) != nil || len(worldDirectory) > 255 {
				continue
			}
			role, worldType, master := runtimeWorldIdentity(reportedWorld, worldDirectory)
			worldName := strings.TrimSpace(reportedWorld.Name)
			if worldName == "" {
				worldName = worldDirectory
			}
			world := World{
				ID: EncodeID(worldDirectory), RoomID: roomID, DirectoryName: worldDirectory, Name: worldName,
				Role: role, Type: worldType, IsMaster: master, ShardID: reportedWorld.ID, ServerPort: reportedWorld.ServerPort,
				TargetIDs: []string{source.TargetID}, UpdatedAt: observedAt,
			}
			if source.Online && !source.Stale {
				world.AvailableTargetIDs = []string{source.TargetID}
			}
			worlds = append(worlds, world)
		}
		room.WorldCount = len(worlds)
		values = append(values, sourceRoomValue{room: room, worlds: worlds, quality: 10})
	}
	return values
}

func runtimeWorldIdentity(value shared.ShardInventoryReport, directory string) (WorldRole, WorldType, bool) {
	master := strings.EqualFold(strings.TrimSpace(value.Role), "master")
	worldType := WorldType(worldidentity.ResolveReportedType(value.Type, directory))
	if master {
		return WorldRoleMaster, worldType, true
	}
	if worldType == WorldTypeCave {
		return WorldRoleCaves, worldType, false
	}
	return WorldRoleCustom, worldType, false
}

func mergeRuntimeSources(sources []RuntimeCatalogSource, local []sourceRoomValue) []catalogRoomValue {
	sort.SliceStable(sources, func(i, j int) bool {
		leftHealthy := sources[i].Online && sources[i].Available && !sources[i].Stale
		rightHealthy := sources[j].Online && sources[j].Available && !sources[j].Stale
		if leftHealthy != rightHealthy {
			return leftHealthy
		}
		if !sources[i].ObservedAt.Equal(sources[j].ObservedAt) {
			return sources[i].ObservedAt.After(sources[j].ObservedAt)
		}
		return sources[i].TargetID < sources[j].TargetID
	})
	values := make(map[string]*catalogRoomValue)
	merge := func(source sourceRoomValue) {
		current := values[source.room.ID]
		if current == nil {
			copy := source.room
			copy.TargetIDs = nil
			copy.AvailableTargetIDs = nil
			current = &catalogRoomValue{Room: copy, MetadataQuality: source.quality, WorldQuality: map[string]int{}}
			values[source.room.ID] = current
		} else if source.quality > current.MetadataQuality {
			targetIDs, availableIDs := current.Room.TargetIDs, current.Room.AvailableTargetIDs
			current.Room = source.room
			current.Room.TargetIDs, current.Room.AvailableTargetIDs = targetIDs, availableIDs
			current.MetadataQuality = source.quality
		}
		current.Room.TargetIDs = mergeIDs(current.Room.TargetIDs, source.room.TargetIDs)
		current.Room.AvailableTargetIDs = mergeIDs(current.Room.AvailableTargetIDs, source.room.AvailableTargetIDs)
		if source.room.UpdatedAt.After(current.Room.UpdatedAt) {
			current.Room.UpdatedAt = source.room.UpdatedAt
		}
		worlds := make(map[string]int, len(current.Worlds))
		for index := range current.Worlds {
			worlds[current.Worlds[index].ID] = index
		}
		for _, world := range source.worlds {
			index, exists := worlds[world.ID]
			if !exists {
				current.Worlds = append(current.Worlds, world)
				worlds[world.ID] = len(current.Worlds) - 1
				current.WorldQuality[world.ID] = source.quality
				continue
			}
			existing := current.Worlds[index]
			if source.quality > current.WorldQuality[world.ID] {
				targetIDs, availableIDs := existing.TargetIDs, existing.AvailableTargetIDs
				existing = world
				existing.TargetIDs, existing.AvailableTargetIDs = targetIDs, availableIDs
				current.WorldQuality[world.ID] = source.quality
			}
			existing.TargetIDs = mergeIDs(existing.TargetIDs, world.TargetIDs)
			existing.AvailableTargetIDs = mergeIDs(existing.AvailableTargetIDs, world.AvailableTargetIDs)
			if world.UpdatedAt.After(existing.UpdatedAt) {
				existing.UpdatedAt = world.UpdatedAt
			}
			current.Worlds[index] = existing
		}
	}
	for _, source := range local {
		merge(source)
	}
	for _, source := range sources {
		for _, room := range runtimeSourceRooms(source) {
			merge(room)
		}
	}
	result := make([]catalogRoomValue, 0, len(values))
	for _, value := range values {
		sortWorlds(value.Worlds)
		value.Room.WorldCount = len(value.Worlds)
		result = append(result, *value)
	}
	sort.Slice(result, func(i, j int) bool {
		if !strings.EqualFold(result[i].Room.Name, result[j].Room.Name) {
			return strings.ToLower(result[i].Room.Name) < strings.ToLower(result[j].Room.Name)
		}
		return result[i].Room.ID < result[j].Room.ID
	})
	return result
}

func mergeIDs(values ...[]string) []string {
	seen := map[string]bool{}
	result := make([]string, 0)
	for _, items := range values {
		for _, item := range items {
			item = strings.TrimSpace(item)
			if item == "" || seen[item] {
				continue
			}
			seen[item] = true
			result = append(result, item)
		}
	}
	sort.Strings(result)
	return result
}

func sortWorlds(worlds []World) {
	sort.SliceStable(worlds, func(i, j int) bool {
		if worlds[i].Role != worlds[j].Role {
			return worldRoleOrder(worlds[i].Role) < worldRoleOrder(worlds[j].Role)
		}
		return strings.ToLower(worlds[i].Name) < strings.ToLower(worlds[j].Name)
	})
}

func encodeIDs(values []string) string {
	payload, _ := json.Marshal(mergeIDs(values))
	return string(payload)
}

func decodeIDs(payload string) []string {
	var values []string
	if json.Unmarshal([]byte(payload), &values) != nil {
		return []string{}
	}
	return mergeIDs(values)
}

func (s *Store) ReplaceRuntimeCatalog(values []catalogRoomValue, confirmedTargets map[string]bool) ([]string, error) {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	now := s.now().UTC()
	tx := s.db.Begin()
	if tx.Error != nil {
		return nil, tx.Error
	}
	rollback := func(err error) ([]string, error) { tx.Rollback(); return nil, err }
	removedRooms := make([]string, 0)
	seenRooms := make(map[string]bool, len(values))
	seenWorlds := make(map[string]bool)
	for _, value := range values {
		seenRooms[value.Room.ID] = true
		if err := s.upsertCatalogRoom(tx, value, now); err != nil {
			return rollback(err)
		}
		for _, world := range value.Worlds {
			key := catalogWorldID(value.Room.ID, world.ID)
			seenWorlds[key] = true
			if err := s.upsertCatalogWorld(tx, world, value.WorldQuality[world.ID], now); err != nil {
				return rollback(err)
			}
		}
	}
	var existingRooms []catalogRoomRecord
	if err := tx.Table(s.catalogRoomsTable).Find(&existingRooms).Error; err != nil {
		return rollback(err)
	}
	for _, existing := range existingRooms {
		if seenRooms[existing.RoomID] {
			continue
		}
		var managed int
		if err := tx.Table(s.table).Where("room_id = ?", existing.RoomID).Count(&managed).Error; err != nil {
			return rollback(err)
		}
		targetIDs := decodeIDs(existing.TargetIDs)
		if managed > 0 && !allTargetsConfirmed(targetIDs, confirmedTargets) {
			if err := tx.Table(s.catalogRoomsTable).Where("room_id = ?", existing.RoomID).Updates(map[string]interface{}{
				"available_target_ids": "[]", "updated_at": now,
			}).Error; err != nil {
				return rollback(err)
			}
			if err := tx.Table(s.catalogWorldsTable).Where("room_id = ?", existing.RoomID).Updates(map[string]interface{}{
				"available_target_ids": "[]", "updated_at": now,
			}).Error; err != nil {
				return rollback(err)
			}
			continue
		}
		if managed > 0 {
			if err := tx.Table(s.table).Where("room_id = ?", existing.RoomID).Delete(&managedRoomRecord{}).Error; err != nil {
				return rollback(err)
			}
			removedRooms = append(removedRooms, existing.RoomID)
		}
		if err := tx.Table(s.catalogWorldsTable).Where("room_id = ?", existing.RoomID).Delete(&catalogWorldRecord{}).Error; err != nil {
			return rollback(err)
		}
		if err := tx.Table(s.catalogRoomsTable).Where("room_id = ?", existing.RoomID).Delete(&catalogRoomRecord{}).Error; err != nil {
			return rollback(err)
		}
	}
	var existingWorlds []catalogWorldRecord
	if err := tx.Table(s.catalogWorldsTable).Find(&existingWorlds).Error; err != nil {
		return rollback(err)
	}
	for _, existing := range existingWorlds {
		if seenRooms[existing.RoomID] && !seenWorlds[existing.CatalogID] {
			if !allTargetsConfirmed(decodeIDs(existing.TargetIDs), confirmedTargets) {
				if err := tx.Table(s.catalogWorldsTable).Where("catalog_id = ?", existing.CatalogID).Updates(map[string]interface{}{
					"available_target_ids": "[]", "updated_at": now,
				}).Error; err != nil {
					return rollback(err)
				}
				continue
			}
			if err := tx.Table(s.catalogWorldsTable).Where("catalog_id = ?", existing.CatalogID).Delete(&catalogWorldRecord{}).Error; err != nil {
				return rollback(err)
			}
		}
	}
	if err := tx.Commit().Error; err != nil {
		return nil, err
	}
	return removedRooms, nil
}

func allTargetsConfirmed(targetIDs []string, confirmedTargets map[string]bool) bool {
	if len(targetIDs) == 0 {
		return false
	}
	for _, targetID := range targetIDs {
		if !confirmedTargets[targetID] {
			return false
		}
	}
	return true
}

func (s *Store) SaveRuntimeCatalogRoom(value catalogRoomValue) error {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	now := s.now().UTC()
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	rollback := func(err error) error { tx.Rollback(); return err }
	if err := s.upsertCatalogRoom(tx, value, now); err != nil {
		return rollback(err)
	}
	seen := make(map[string]bool, len(value.Worlds))
	for _, world := range value.Worlds {
		key := catalogWorldID(value.Room.ID, world.ID)
		seen[key] = true
		if err := s.upsertCatalogWorld(tx, world, value.WorldQuality[world.ID], now); err != nil {
			return rollback(err)
		}
	}
	var existing []catalogWorldRecord
	if err := tx.Table(s.catalogWorldsTable).Where("room_id = ?", value.Room.ID).Find(&existing).Error; err != nil {
		return rollback(err)
	}
	for _, world := range existing {
		if !seen[world.CatalogID] {
			if err := tx.Table(s.catalogWorldsTable).Where("catalog_id = ?", world.CatalogID).Delete(&catalogWorldRecord{}).Error; err != nil {
				return rollback(err)
			}
		}
	}
	return tx.Commit().Error
}

func (s *Store) RemoveRuntimeCatalogRoom(roomID string) error {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	if err := tx.Table(s.catalogWorldsTable).Where("room_id = ?", roomID).Delete(&catalogWorldRecord{}).Error; err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Table(s.catalogRoomsTable).Where("room_id = ?", roomID).Delete(&catalogRoomRecord{}).Error; err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit().Error
}

func (s *Store) upsertCatalogRoom(tx *gorm.DB, value catalogRoomValue, now time.Time) error {
	room := value.Room
	record := catalogRoomRecord{
		RoomID: room.ID, DirectoryName: room.DirectoryName, Name: room.Name, Description: room.Description,
		GameMode: room.GameMode, MaxPlayers: room.MaxPlayers, PvP: room.PvP, PasswordProtected: room.PasswordProtected,
		TargetIDs: encodeIDs(room.TargetIDs), AvailableTargetIDs: encodeIDs(room.AvailableTargetIDs),
		MetadataQuality: value.MetadataQuality, ObservedAt: room.UpdatedAt.UTC(), UpdatedAt: now,
	}
	var existing catalogRoomRecord
	result := tx.Table(s.catalogRoomsTable).Where("room_id = ?", room.ID).First(&existing)
	if gorm.IsRecordNotFoundError(result.Error) {
		record.CreatedAt = now
		return tx.Table(s.catalogRoomsTable).Create(&record).Error
	}
	if result.Error != nil {
		return result.Error
	}
	if existing.MetadataQuality > record.MetadataQuality {
		record.Name, record.Description, record.GameMode = existing.Name, existing.Description, existing.GameMode
		record.MaxPlayers, record.PvP, record.PasswordProtected = existing.MaxPlayers, existing.PvP, existing.PasswordProtected
		record.MetadataQuality = existing.MetadataQuality
	}
	return tx.Table(s.catalogRoomsTable).Where("room_id = ?", room.ID).Updates(map[string]interface{}{
		"directory_name": record.DirectoryName, "name": record.Name, "description": record.Description,
		"game_mode": record.GameMode, "max_players": record.MaxPlayers, "pvp": record.PvP,
		"password_protected": record.PasswordProtected, "target_ids": record.TargetIDs,
		"available_target_ids": record.AvailableTargetIDs, "metadata_quality": record.MetadataQuality,
		"observed_at": record.ObservedAt, "updated_at": now,
	}).Error
}

func (s *Store) upsertCatalogWorld(tx *gorm.DB, world World, quality int, now time.Time) error {
	record := catalogWorldRecord{
		CatalogID: catalogWorldID(world.RoomID, world.ID), RoomID: world.RoomID, WorldID: world.ID,
		DirectoryName: world.DirectoryName, Name: world.Name, Role: string(world.Role), Type: string(world.Type),
		IsMaster: world.IsMaster, ShardID: world.ShardID, ServerPort: world.ServerPort, TargetIDs: encodeIDs(world.TargetIDs),
		AvailableTargetIDs: encodeIDs(world.AvailableTargetIDs), MetadataQuality: quality,
		ObservedAt: world.UpdatedAt.UTC(), UpdatedAt: now,
	}
	var existing catalogWorldRecord
	result := tx.Table(s.catalogWorldsTable).Where("catalog_id = ?", record.CatalogID).First(&existing)
	if gorm.IsRecordNotFoundError(result.Error) {
		record.CreatedAt = now
		return tx.Table(s.catalogWorldsTable).Create(&record).Error
	}
	if result.Error != nil {
		return result.Error
	}
	if existing.MetadataQuality > record.MetadataQuality {
		record.DirectoryName, record.Name, record.Role, record.Type = existing.DirectoryName, existing.Name, existing.Role, existing.Type
		record.IsMaster, record.ShardID, record.ServerPort, record.MetadataQuality = existing.IsMaster, existing.ShardID, existing.ServerPort, existing.MetadataQuality
	}
	return tx.Table(s.catalogWorldsTable).Where("catalog_id = ?", record.CatalogID).Updates(map[string]interface{}{
		"room_id": record.RoomID, "world_id": record.WorldID, "directory_name": record.DirectoryName,
		"name": record.Name, "role": record.Role, "type": record.Type, "is_master": record.IsMaster,
		"shard_id": record.ShardID, "server_port": record.ServerPort, "target_ids": record.TargetIDs,
		"available_target_ids": record.AvailableTargetIDs, "metadata_quality": record.MetadataQuality,
		"observed_at": record.ObservedAt, "updated_at": now,
	}).Error
}

func (s *Store) CatalogRooms() ([]Room, error) {
	var records []catalogRoomRecord
	if err := s.db.Table(s.catalogRoomsTable).Order("name ASC, room_id ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	managed, err := s.managedRoomIDs()
	if err != nil {
		return nil, err
	}
	worldCounts := map[string]int{}
	var worlds []catalogWorldRecord
	if err := s.db.Table(s.catalogWorldsTable).Find(&worlds).Error; err != nil {
		return nil, err
	}
	for _, world := range worlds {
		worldCounts[world.RoomID]++
	}
	items := make([]Room, 0, len(records))
	for _, record := range records {
		items = append(items, roomFromCatalogRecord(record, managed[record.RoomID], worldCounts[record.RoomID]))
	}
	return items, nil
}

func (s *Store) CatalogRoom(roomID string) (Room, error) {
	var record catalogRoomRecord
	result := s.db.Table(s.catalogRoomsTable).Where("room_id = ?", roomID).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Room{}, ErrRoomNotFound
	}
	if result.Error != nil {
		return Room{}, result.Error
	}
	managed, err := s.IsManaged(roomID)
	if err != nil {
		return Room{}, err
	}
	var worldCount int
	if err := s.db.Table(s.catalogWorldsTable).Where("room_id = ?", roomID).Count(&worldCount).Error; err != nil {
		return Room{}, err
	}
	return roomFromCatalogRecord(record, managed, worldCount), nil
}

func (s *Store) CatalogWorlds(roomID string) ([]World, error) {
	var records []catalogWorldRecord
	if err := s.db.Table(s.catalogWorldsTable).Where("room_id = ?", roomID).Find(&records).Error; err != nil {
		return nil, err
	}
	items := make([]World, 0, len(records))
	for _, record := range records {
		items = append(items, worldFromCatalogRecord(record))
	}
	sortWorlds(items)
	return items, nil
}

func (s *Store) CatalogWorld(roomID, worldID string) (World, error) {
	var record catalogWorldRecord
	result := s.db.Table(s.catalogWorldsTable).Where("catalog_id = ?", catalogWorldID(roomID, worldID)).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return World{}, ErrWorldNotFound
	}
	if result.Error != nil {
		return World{}, result.Error
	}
	return worldFromCatalogRecord(record), nil
}

func (s *Store) managedRoomIDs() (map[string]bool, error) {
	var records []managedRoomRecord
	if err := s.db.Table(s.table).Find(&records).Error; err != nil {
		return nil, err
	}
	values := make(map[string]bool, len(records))
	for _, record := range records {
		values[record.RoomID] = true
	}
	return values, nil
}

func roomFromCatalogRecord(record catalogRoomRecord, managed bool, worldCount int) Room {
	return Room{
		ID: record.RoomID, DirectoryName: record.DirectoryName, Name: record.Name,
		Description: record.Description, GameMode: record.GameMode, MaxPlayers: record.MaxPlayers,
		PvP: record.PvP, PasswordProtected: record.PasswordProtected, Managed: managed, WorldCount: worldCount,
		TargetIDs: decodeIDs(record.TargetIDs), AvailableTargetIDs: decodeIDs(record.AvailableTargetIDs),
		UpdatedAt: record.ObservedAt.UTC(),
	}
}

func worldFromCatalogRecord(record catalogWorldRecord) World {
	role := WorldRole(record.Role)
	if role != WorldRoleMaster && role != WorldRoleCaves && role != WorldRoleCustom {
		role = WorldRoleCustom
	}
	worldType := WorldType(record.Type)
	if worldType != WorldTypeForest && worldType != WorldTypeCave && worldType != WorldTypeUnknown {
		worldType = WorldTypeUnknown
	}
	return World{
		ID: record.WorldID, RoomID: record.RoomID, DirectoryName: record.DirectoryName, Name: record.Name,
		Role: role, Type: worldType, IsMaster: record.IsMaster, ShardID: record.ShardID, ServerPort: record.ServerPort,
		TargetIDs: decodeIDs(record.TargetIDs), AvailableTargetIDs: decodeIDs(record.AvailableTargetIDs),
		UpdatedAt: record.ObservedAt.UTC(),
	}
}

func localSourceRooms(catalog *Catalog) ([]sourceRoomValue, error) {
	items, err := catalog.List()
	if err != nil {
		return nil, err
	}
	result := make([]sourceRoomValue, 0, len(items))
	for _, room := range items {
		worlds, worldsErr := catalog.Worlds(room.ID)
		if worldsErr != nil {
			return nil, worldsErr
		}
		room.TargetIDs, room.AvailableTargetIDs = []string{"local"}, []string{"local"}
		for index := range worlds {
			worlds[index].TargetIDs, worlds[index].AvailableTargetIDs = []string{"local"}, []string{"local"}
		}
		result = append(result, sourceRoomValue{room: room, worlds: worlds, quality: 100})
	}
	return result, nil
}

func mergeRoomValues(primary, secondary Room) Room {
	primary.TargetIDs = mergeIDs(primary.TargetIDs, secondary.TargetIDs)
	primary.AvailableTargetIDs = mergeIDs(primary.AvailableTargetIDs, secondary.AvailableTargetIDs)
	primary.Managed = primary.Managed || secondary.Managed
	if secondary.WorldCount > primary.WorldCount {
		primary.WorldCount = secondary.WorldCount
	}
	if secondary.UpdatedAt.After(primary.UpdatedAt) {
		primary.UpdatedAt = secondary.UpdatedAt
	}
	return primary
}

func mergeWorldValues(primary, secondary World) World {
	primary.TargetIDs = mergeIDs(primary.TargetIDs, secondary.TargetIDs)
	primary.AvailableTargetIDs = mergeIDs(primary.AvailableTargetIDs, secondary.AvailableTargetIDs)
	if secondary.UpdatedAt.After(primary.UpdatedAt) {
		primary.UpdatedAt = secondary.UpdatedAt
	}
	return primary
}

func catalogLookupError(err error, notFound error) error {
	if err == nil || errors.Is(err, notFound) {
		return err
	}
	return fmt.Errorf("read runtime catalog: %w", err)
}
