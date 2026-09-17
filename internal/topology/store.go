package topology

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/rooms"

	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
)

type topologyRecord struct {
	RoomID            string `gorm:"type:varchar(255);primary_key"`
	Revision          string `gorm:"type:char(36);not null;index"`
	Placements        string `gorm:"type:text;not null"`
	ShardLinks        string `gorm:"type:text"`
	AppliedShardLinks string `gorm:"type:text"`
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type Store struct {
	db                    *gorm.DB
	table                 string
	providersTable        string
	environmentsTable     string
	networkProfilesTable  string
	portReservationsTable string
	portLocksTable        string
	cpuAllocationsTable   string
	now                   func() time.Time
	portMu                sync.Mutex
}

func NewStore(db *gorm.DB, tablePrefix string) *Store {
	prefix := strings.TrimSpace(tablePrefix)
	return &Store{
		db: db, table: prefix + "room_topology", providersTable: prefix + "runtime_provider",
		environmentsTable: prefix + "execution_environment", networkProfilesTable: prefix + "network_profile",
		portReservationsTable: prefix + "port_reservation", cpuAllocationsTable: prefix + "cpu_allocation", now: time.Now,
		portLocksTable: prefix + "port_allocation_lock",
	}
}

func (s *Store) Migrate() error {
	if err := s.db.Table(s.table).AutoMigrate(&topologyRecord{}).Error; err != nil {
		return fmt.Errorf("migrate room topology: %w", err)
	}
	for _, migration := range []struct {
		table string
		model interface{}
	}{
		{s.providersTable, &runtimeProviderRecord{}},
		{s.environmentsTable, &executionEnvironmentRecord{}},
		{s.networkProfilesTable, &networkProfileRecord{}},
		{s.portReservationsTable, &portReservationRecord{}},
		{s.portLocksTable, &portAllocationLockRecord{}},
		{s.cpuAllocationsTable, &cpuAllocationRecord{}},
	} {
		if err := s.db.Table(migration.table).AutoMigrate(migration.model).Error; err != nil {
			return fmt.Errorf("migrate %s: %w", migration.table, err)
		}
	}
	return nil
}

func (s *Store) Ensure(roomID string, worldIDs []string) (record, error) {
	worldIDs = normalizedWorldIDs(worldIDs)
	worlds := make([]rooms.World, 0, len(worldIDs))
	for _, worldID := range worldIDs {
		worlds = append(worlds, rooms.World{ID: worldID})
	}
	return s.EnsureWorlds(roomID, worlds)
}

func (s *Store) EnsureWorlds(roomID string, worlds []rooms.World) (record, error) {
	worlds = normalizedWorlds(worlds)
	for attempt := 0; attempt < 3; attempt++ {
		current, err := s.load(roomID)
		if gorm.IsRecordNotFoundError(err) {
			placements := make([]storedPlacement, 0, len(worlds))
			for _, world := range worlds {
				placements = append(placements, placementForWorld(world))
			}
			now := s.now().UTC()
			created := record{RoomID: roomID, Revision: uuid.NewString(), Placements: placements, CreatedAt: now, UpdatedAt: now}
			if createErr := s.create(created); createErr == nil {
				return created, nil
			} else if _, loadErr := s.load(roomID); loadErr != nil {
				// A concurrent Ensure may have won the insert. Retry only when
				// the row now exists; persistent database errors must surface.
				if gorm.IsRecordNotFoundError(loadErr) {
					return record{}, createErr
				}
				return record{}, loadErr
			}
			continue
		}
		if err != nil {
			return record{}, err
		}
		next, changed := reconcileWorldPlacements(current.Placements, worlds)
		if !changed {
			return current, nil
		}
		saved, saveErr := s.Save(roomID, current.Revision, next)
		if saveErr == nil {
			return saved, nil
		}
		if _, conflict := saveErr.(*RevisionConflictError); !conflict {
			return record{}, saveErr
		}
	}
	current, err := s.load(roomID)
	if err != nil {
		return record{}, err
	}
	return record{}, &RevisionConflictError{CurrentRevision: current.Revision}
}

func (s *Store) Save(roomID, expectedRevision string, placements []storedPlacement) (record, error) {
	current, err := s.load(roomID)
	if err != nil {
		return record{}, err
	}
	if current.Revision != expectedRevision {
		return record{}, &RevisionConflictError{CurrentRevision: current.Revision}
	}
	return s.SavePlan(roomID, expectedRevision, placements, current.ShardLinks, current.AppliedShardLinks)
}

func (s *Store) SavePlan(roomID, expectedRevision string, placements []storedPlacement, shardLinks, appliedShardLinks []storedShardLink) (record, error) {
	placements = normalizedPlacements(placements)
	payload, err := json.Marshal(placements)
	if err != nil {
		return record{}, err
	}
	linksPayload, err := json.Marshal(normalizedStoredShardLinks(shardLinks))
	if err != nil {
		return record{}, err
	}
	appliedLinksPayload, err := json.Marshal(normalizedStoredShardLinks(appliedShardLinks))
	if err != nil {
		return record{}, err
	}
	now := s.now().UTC()
	nextRevision := uuid.NewString()
	result := s.db.Table(s.table).Where("room_id = ? AND revision = ?", roomID, expectedRevision).Updates(map[string]interface{}{
		"revision": nextRevision, "placements": string(payload), "shard_links": string(linksPayload),
		"applied_shard_links": string(appliedLinksPayload), "updated_at": now,
	})
	if result.Error != nil {
		return record{}, result.Error
	}
	if result.RowsAffected == 0 {
		current, loadErr := s.load(roomID)
		if loadErr != nil {
			return record{}, loadErr
		}
		return record{}, &RevisionConflictError{CurrentRevision: current.Revision}
	}
	current, err := s.load(roomID)
	if err != nil {
		return record{}, err
	}
	return current, nil
}

func (s *Store) Records() ([]record, error) {
	var values []topologyRecord
	if err := s.db.Table(s.table).Order("room_id ASC").Find(&values).Error; err != nil {
		return nil, err
	}
	items := make([]record, 0, len(values))
	for _, value := range values {
		item, err := recordFromDatabase(value)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Store) create(value record) error {
	payload, err := json.Marshal(normalizedPlacements(value.Placements))
	if err != nil {
		return err
	}
	linksPayload, err := json.Marshal(normalizedStoredShardLinks(value.ShardLinks))
	if err != nil {
		return err
	}
	appliedLinksPayload, err := json.Marshal(normalizedStoredShardLinks(value.AppliedShardLinks))
	if err != nil {
		return err
	}
	return s.db.Table(s.table).Create(&topologyRecord{
		RoomID: value.RoomID, Revision: value.Revision, Placements: string(payload), ShardLinks: string(linksPayload),
		AppliedShardLinks: string(appliedLinksPayload),
		CreatedAt:         value.CreatedAt.UTC(), UpdatedAt: value.UpdatedAt.UTC(),
	}).Error
}

func (s *Store) load(roomID string) (record, error) {
	var value topologyRecord
	result := s.db.Table(s.table).Where("room_id = ?", roomID).First(&value)
	if result.Error != nil {
		return record{}, result.Error
	}
	return recordFromDatabase(value)
}

func recordFromDatabase(value topologyRecord) (record, error) {
	var placements []storedPlacement
	if err := json.Unmarshal([]byte(value.Placements), &placements); err != nil {
		return record{}, fmt.Errorf("decode room topology: %w", err)
	}
	var shardLinks []storedShardLink
	if strings.TrimSpace(value.ShardLinks) != "" {
		if err := json.Unmarshal([]byte(value.ShardLinks), &shardLinks); err != nil {
			return record{}, fmt.Errorf("decode room shard links: %w", err)
		}
	}
	var appliedShardLinks []storedShardLink
	if strings.TrimSpace(value.AppliedShardLinks) != "" {
		if err := json.Unmarshal([]byte(value.AppliedShardLinks), &appliedShardLinks); err != nil {
			return record{}, fmt.Errorf("decode applied room shard links: %w", err)
		}
	} else if placementsAligned(placements) {
		// Rows created before applied links were persisted are safe to inherit
		// only when there is no unfinished placement transition.
		appliedShardLinks = shardLinks
	}
	return record{
		RoomID: value.RoomID, Revision: value.Revision, Placements: normalizedPlacements(placements),
		ShardLinks: normalizedStoredShardLinks(shardLinks), AppliedShardLinks: normalizedStoredShardLinks(appliedShardLinks),
		CreatedAt: value.CreatedAt.UTC(), UpdatedAt: value.UpdatedAt.UTC(),
	}, nil
}

func placementsAligned(values []storedPlacement) bool {
	for _, value := range values {
		if !sameEndpoint(value.DesiredTargetID, value.DesiredInstallationID, value.AppliedTargetID, value.AppliedInstallationID) {
			return false
		}
	}
	return true
}

func normalizedStoredShardLinks(values []storedShardLink) []storedShardLink {
	result := append([]storedShardLink(nil), values...)
	for index := range result {
		result[index].SourceTargetID = strings.TrimSpace(result[index].SourceTargetID)
		result[index].SourceInstallationID = strings.TrimSpace(result[index].SourceInstallationID)
		result[index].MasterTargetID = strings.TrimSpace(result[index].MasterTargetID)
		result[index].MasterInstallationID = strings.TrimSpace(result[index].MasterInstallationID)
		result[index].Address = strings.TrimSpace(strings.Trim(result[index].Address, "[]"))
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].SourceTargetID != result[j].SourceTargetID {
			return result[i].SourceTargetID < result[j].SourceTargetID
		}
		return result[i].SourceInstallationID < result[j].SourceInstallationID
	})
	return result
}

func normalizedWorldIDs(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func normalizedPlacements(values []storedPlacement) []storedPlacement {
	result := append([]storedPlacement(nil), values...)
	for index := range result {
		result[index].WorldID = strings.TrimSpace(result[index].WorldID)
		result[index].WorldDirectoryName = strings.TrimSpace(result[index].WorldDirectoryName)
		result[index].WorldName = strings.TrimSpace(result[index].WorldName)
		if result[index].WorldRole != rooms.WorldRoleMaster && result[index].WorldRole != rooms.WorldRoleCaves && result[index].WorldRole != rooms.WorldRoleCustom {
			result[index].WorldRole = ""
		}
		result[index].DesiredTargetID = strings.TrimSpace(result[index].DesiredTargetID)
		result[index].AppliedTargetID = strings.TrimSpace(result[index].AppliedTargetID)
		result[index].DesiredInstallationID = strings.TrimSpace(result[index].DesiredInstallationID)
		result[index].AppliedInstallationID = strings.TrimSpace(result[index].AppliedInstallationID)
		if result[index].DesiredTargetID == "" {
			result[index].DesiredTargetID = "local"
		}
		if result[index].AppliedTargetID == "" {
			result[index].AppliedTargetID = "local"
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].WorldID < result[j].WorldID })
	return result
}

func reconcilePlacements(current []storedPlacement, worldIDs []string) ([]storedPlacement, bool) {
	worldIDs = normalizedWorldIDs(worldIDs)
	worlds := make([]rooms.World, 0, len(worldIDs))
	for _, worldID := range worldIDs {
		worlds = append(worlds, rooms.World{ID: worldID})
	}
	return reconcileWorldPlacements(current, worlds)
}

func reconcileWorldPlacements(current []storedPlacement, worlds []rooms.World) ([]storedPlacement, bool) {
	byWorld := make(map[string]storedPlacement, len(current))
	for _, placement := range current {
		byWorld[placement.WorldID] = placement
	}
	next := make([]storedPlacement, 0, len(worlds)+len(current))
	for _, world := range normalizedWorlds(worlds) {
		placement, exists := byWorld[world.ID]
		if !exists {
			placement = placementForWorld(world)
		} else {
			placement = mergeWorldMetadata(placement, world)
		}
		next = append(next, placement)
		delete(byWorld, world.ID)
	}
	for _, placement := range byWorld {
		if placement.AppliedTargetID != localTargetID || placement.DesiredTargetID != localTargetID ||
			placement.AppliedInstallationID != placement.DesiredInstallationID {
			next = append(next, placement)
		}
	}
	next = normalizedPlacements(next)
	current = normalizedPlacements(current)
	if len(next) != len(current) {
		return next, true
	}
	for index := range next {
		if next[index] != current[index] {
			return next, true
		}
	}
	return next, false
}

func normalizedWorlds(values []rooms.World) []rooms.World {
	seen := make(map[string]bool, len(values))
	result := make([]rooms.World, 0, len(values))
	for _, value := range values {
		value.ID = strings.TrimSpace(value.ID)
		if value.ID == "" || seen[value.ID] {
			continue
		}
		seen[value.ID] = true
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func placementForWorld(world rooms.World) storedPlacement {
	targetID := defaultPlacementTarget(world.TargetIDs)
	return mergeWorldMetadata(storedPlacement{
		WorldID: world.ID, DesiredTargetID: targetID, AppliedTargetID: targetID,
	}, world)
}

func defaultPlacementTarget(targetIDs []string) string {
	values := make([]string, 0, len(targetIDs))
	seen := make(map[string]bool, len(targetIDs))
	for _, targetID := range targetIDs {
		targetID = strings.TrimSpace(targetID)
		if targetID == "" || seen[targetID] {
			continue
		}
		if targetID == localTargetID {
			return localTargetID
		}
		seen[targetID] = true
		values = append(values, targetID)
	}
	if len(values) == 0 {
		return localTargetID
	}
	sort.Strings(values)
	return values[0]
}

func mergeWorldMetadata(placement storedPlacement, world rooms.World) storedPlacement {
	if value := strings.TrimSpace(world.DirectoryName); value != "" {
		placement.WorldDirectoryName = value
	}
	if value := strings.TrimSpace(world.Name); value != "" {
		placement.WorldName = value
	}
	if world.Role == rooms.WorldRoleMaster || world.Role == rooms.WorldRoleCaves || world.Role == rooms.WorldRoleCustom {
		placement.WorldRole = world.Role
	}
	return placement
}
