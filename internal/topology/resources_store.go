package topology

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"dont/internal/agents"
	"dont/shared"

	"github.com/jinzhu/gorm"
)

type runtimeProviderRecord struct {
	ID           string `gorm:"type:varchar(64);primary_key"`
	TargetID     string `gorm:"type:varchar(160);not null;unique_index"`
	Kind         string `gorm:"type:varchar(24);not null"`
	DisplayName  string `gorm:"type:varchar(255);not null"`
	OS           string `gorm:"type:varchar(64);not null"`
	Arch         string `gorm:"type:varchar(64);not null"`
	Online       bool
	Capabilities string `gorm:"type:text;not null"`
	ObservedAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type executionEnvironmentRecord struct {
	ID               string `gorm:"type:varchar(64);primary_key"`
	ProviderID       string `gorm:"type:varchar(64);not null;index"`
	TargetID         string `gorm:"type:varchar(160);not null;unique_index"`
	Kind             string `gorm:"type:varchar(24);not null"`
	Driver           string `gorm:"type:varchar(32);not null"`
	NetworkProfileID string `gorm:"type:varchar(64);not null"`
	CPU              string `gorm:"type:text;not null"`
	ObservedAt       *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type networkProfileRecord struct {
	ID               string `gorm:"type:varchar(64);primary_key"`
	EnvironmentID    string `gorm:"type:varchar(64);not null;unique_index"`
	Name             string `gorm:"type:varchar(100);not null"`
	Mode             string `gorm:"type:varchar(24);not null"`
	ScopeID          string `gorm:"type:varchar(160);not null;index"`
	BindAddress      string `gorm:"type:varchar(255);not null"`
	AdvertiseAddress string `gorm:"type:varchar(255);not null"`
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type portReservationRecord struct {
	ID               string `gorm:"type:varchar(64);primary_key"`
	EnvironmentID    string `gorm:"type:varchar(64);not null;index"`
	NetworkProfileID string `gorm:"type:varchar(64);not null;index"`
	ScopeID          string `gorm:"type:varchar(160);not null;index"`
	TargetID         string `gorm:"type:varchar(160);not null;index"`
	RoomID           string `gorm:"type:varchar(255);index"`
	WorldID          string `gorm:"type:varchar(255);index"`
	Cluster          string `gorm:"type:varchar(255);not null"`
	Shard            string `gorm:"type:varchar(255);not null"`
	Purpose          string `gorm:"type:varchar(40);not null"`
	Protocol         string `gorm:"type:varchar(8);not null"`
	BindAddress      string `gorm:"type:varchar(255);not null"`
	Port             int    `gorm:"not null;index"`
	State            string `gorm:"type:varchar(24);not null;index"`
	Managed          bool
	LeaseID          string `gorm:"type:varchar(64);index"`
	OwnerID          string `gorm:"type:varchar(255);index"`
	ExpiresAt        *time.Time
	ActivatedAt      *time.Time
	ReleasedAt       *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type portAllocationLockRecord struct {
	ScopeID   string `gorm:"type:varchar(160);primary_key"`
	Revision  uint64 `gorm:"not null"`
	UpdatedAt time.Time
}

type cpuAllocationRecord struct {
	ID                  string `gorm:"type:varchar(64);primary_key"`
	EnvironmentID       string `gorm:"type:varchar(64);not null;index"`
	TargetID            string `gorm:"type:varchar(160);not null;index"`
	RoomID              string `gorm:"type:varchar(255);not null;index:cpu_allocation_owner"`
	WorldID             string `gorm:"type:varchar(255);not null;index:cpu_allocation_owner"`
	Policy              string `gorm:"type:varchar(24);not null"`
	LogicalCPUIds       string `gorm:"type:text;not null"`
	PhysicalCoreKeys    string `gorm:"type:text;not null"`
	AllowSMTSiblingRisk bool
	Warnings            string `gorm:"type:text;not null"`
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

func providerResourceID(targetID string) string    { return stableResourceID("provider", targetID) }
func environmentResourceID(targetID string) string { return stableResourceID("environment", targetID) }
func networkResourceID(targetID string) string     { return stableResourceID("network", targetID) }
func allocationResourceID(roomID, worldID string) string {
	return stableResourceID("cpu", roomID+"\x00"+worldID)
}

func stableResourceID(prefix, identity string) string {
	digest := sha256.Sum256([]byte(identity))
	return prefix + "-" + hex.EncodeToString(digest[:12])
}

func (s *Store) SyncRuntimeCatalog(inventories []agents.RuntimeTargetInventory) error {
	now := s.now().UTC()
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	rollback := func(err error) error { tx.Rollback(); return err }
	seen := make([]string, 0, len(inventories))
	for _, inventory := range inventories {
		target := inventory.Target
		if strings.TrimSpace(target.ID) == "" {
			continue
		}
		seen = append(seen, target.ID)
		providerKind := ProviderAgent
		if target.Kind == agents.RuntimeKindLocal {
			providerKind = ProviderLocal
		}
		capabilities, _ := json.Marshal(target.Capabilities)
		provider := runtimeProviderRecord{
			ID: providerResourceID(target.ID), TargetID: target.ID, Kind: string(providerKind), DisplayName: target.Name,
			OS: target.OS, Arch: target.Arch, Online: target.Online, Capabilities: string(capabilities), ObservedAt: inventory.ObservedAt, UpdatedAt: now,
		}
		if err := upsertProvider(tx, s.providersTable, provider, now); err != nil {
			return rollback(err)
		}
		cpuPayload, _ := json.Marshal(inventory.Inventory.CPU)
		environment := executionEnvironmentRecord{
			ID: environmentResourceID(target.ID), ProviderID: provider.ID, TargetID: target.ID,
			Kind: string(EnvironmentNative), Driver: "tmux", NetworkProfileID: networkResourceID(target.ID),
			CPU: string(cpuPayload), ObservedAt: inventory.ObservedAt, UpdatedAt: now,
		}
		if err := upsertEnvironment(tx, s.environmentsTable, environment, now); err != nil {
			return rollback(err)
		}
		profile := networkProfileRecord{
			ID: environment.NetworkProfileID, EnvironmentID: environment.ID, Name: target.Name + " 主机网络",
			Mode: string(NetworkHost), ScopeID: "host:" + target.ID, BindAddress: "0.0.0.0", UpdatedAt: now,
		}
		if err := ensureNetworkProfile(tx, s.networkProfilesTable, profile, now); err != nil {
			return rollback(err)
		}
	}
	if len(seen) == 0 {
		if err := tx.Table(s.providersTable).Delete(&runtimeProviderRecord{}).Error; err != nil {
			return rollback(err)
		}
		if err := tx.Table(s.environmentsTable).Delete(&executionEnvironmentRecord{}).Error; err != nil {
			return rollback(err)
		}
		if err := tx.Table(s.networkProfilesTable).Delete(&networkProfileRecord{}).Error; err != nil {
			return rollback(err)
		}
	} else {
		var removed []executionEnvironmentRecord
		if err := tx.Table(s.environmentsTable).Where("target_id NOT IN (?)", seen).Find(&removed).Error; err != nil {
			return rollback(err)
		}
		for _, item := range removed {
			if err := tx.Table(s.networkProfilesTable).Where("environment_id = ?", item.ID).Delete(&networkProfileRecord{}).Error; err != nil {
				return rollback(err)
			}
		}
		if err := tx.Table(s.environmentsTable).Where("target_id NOT IN (?)", seen).Delete(&executionEnvironmentRecord{}).Error; err != nil {
			return rollback(err)
		}
		if err := tx.Table(s.providersTable).Where("target_id NOT IN (?)", seen).Delete(&runtimeProviderRecord{}).Error; err != nil {
			return rollback(err)
		}
	}
	return tx.Commit().Error
}

func upsertProvider(tx *gorm.DB, table string, value runtimeProviderRecord, now time.Time) error {
	var existing runtimeProviderRecord
	result := tx.Table(table).Where("id = ?", value.ID).First(&existing)
	if gorm.IsRecordNotFoundError(result.Error) {
		value.CreatedAt = now
		return tx.Table(table).Create(&value).Error
	}
	if result.Error != nil {
		return result.Error
	}
	return tx.Table(table).Where("id = ?", value.ID).Updates(map[string]interface{}{
		"target_id": value.TargetID, "kind": value.Kind, "display_name": value.DisplayName, "os": value.OS, "arch": value.Arch, "online": value.Online,
		"capabilities": value.Capabilities, "observed_at": value.ObservedAt, "updated_at": now,
	}).Error
}

func upsertEnvironment(tx *gorm.DB, table string, value executionEnvironmentRecord, now time.Time) error {
	var existing executionEnvironmentRecord
	result := tx.Table(table).Where("id = ?", value.ID).First(&existing)
	if gorm.IsRecordNotFoundError(result.Error) {
		value.CreatedAt = now
		return tx.Table(table).Create(&value).Error
	}
	if result.Error != nil {
		return result.Error
	}
	return tx.Table(table).Where("id = ?", value.ID).Updates(map[string]interface{}{
		"provider_id": value.ProviderID, "target_id": value.TargetID, "kind": value.Kind, "driver": value.Driver,
		"network_profile_id": value.NetworkProfileID, "cpu": value.CPU, "observed_at": value.ObservedAt, "updated_at": now,
	}).Error
}

func ensureNetworkProfile(tx *gorm.DB, table string, value networkProfileRecord, now time.Time) error {
	var existing networkProfileRecord
	result := tx.Table(table).Where("id = ?", value.ID).First(&existing)
	if gorm.IsRecordNotFoundError(result.Error) {
		value.CreatedAt = now
		return tx.Table(table).Create(&value).Error
	}
	if result.Error != nil {
		return result.Error
	}
	return tx.Table(table).Where("id = ?", value.ID).Updates(map[string]interface{}{
		"environment_id": value.EnvironmentID, "mode": value.Mode, "scope_id": value.ScopeID, "updated_at": now,
	}).Error
}

func (s *Store) RuntimeResources() ([]RuntimeProvider, []ExecutionEnvironment, []NetworkProfile, []PortReservation, []CPUAllocation, error) {
	var providerRecords []runtimeProviderRecord
	var environmentRecords []executionEnvironmentRecord
	var profileRecords []networkProfileRecord
	var reservationRecords []portReservationRecord
	var allocationRecords []cpuAllocationRecord
	for _, query := range []struct {
		table string
		out   interface{}
		order string
	}{
		{s.providersTable, &providerRecords, "display_name ASC, id ASC"},
		{s.environmentsTable, &environmentRecords, "target_id ASC, id ASC"},
		{s.networkProfilesTable, &profileRecords, "name ASC, id ASC"},
		{s.portReservationsTable, &reservationRecords, "scope_id ASC, port ASC, purpose ASC"},
		{s.cpuAllocationsTable, &allocationRecords, "target_id ASC, room_id ASC, world_id ASC"},
	} {
		if err := s.db.Table(query.table).Order(query.order).Find(query.out).Error; err != nil {
			return nil, nil, nil, nil, nil, err
		}
	}
	providers := make([]RuntimeProvider, 0, len(providerRecords))
	for _, record := range providerRecords {
		var capabilities []string
		_ = json.Unmarshal([]byte(record.Capabilities), &capabilities)
		providers = append(providers, RuntimeProvider{ID: record.ID, TargetID: record.TargetID, Kind: ProviderKind(record.Kind), DisplayName: record.DisplayName, OS: record.OS, Arch: record.Arch, Online: record.Online, Capabilities: capabilities, ObservedAt: record.ObservedAt, UpdatedAt: record.UpdatedAt.UTC()})
	}
	environments := make([]ExecutionEnvironment, 0, len(environmentRecords))
	for _, record := range environmentRecords {
		var cpu shared.CPUInventory
		_ = json.Unmarshal([]byte(record.CPU), &cpu)
		environments = append(environments, ExecutionEnvironment{ID: record.ID, ProviderID: record.ProviderID, TargetID: record.TargetID, Kind: EnvironmentKind(record.Kind), Driver: record.Driver, NetworkProfileID: record.NetworkProfileID, CPU: cpu, ObservedAt: record.ObservedAt, UpdatedAt: record.UpdatedAt.UTC()})
	}
	profiles := make([]NetworkProfile, 0, len(profileRecords))
	for _, record := range profileRecords {
		profiles = append(profiles, NetworkProfile{ID: record.ID, EnvironmentID: record.EnvironmentID, Name: record.Name, Mode: NetworkMode(record.Mode), ScopeID: record.ScopeID, BindAddress: record.BindAddress, AdvertiseAddress: record.AdvertiseAddress, UpdatedAt: record.UpdatedAt.UTC()})
	}
	reservations := make([]PortReservation, 0, len(reservationRecords))
	for _, record := range reservationRecords {
		reservations = append(reservations, reservationFromRecord(record))
	}
	allocations := make([]CPUAllocation, 0, len(allocationRecords))
	for _, record := range allocationRecords {
		allocations = append(allocations, allocationFromRecord(record))
	}
	return providers, environments, profiles, reservations, allocations, nil
}

func (s *Store) ReplacePortReservations(values []PortReservation) error {
	now := s.now().UTC()
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	var existing []portReservationRecord
	if err := tx.Table(s.portReservationsTable).Find(&existing).Error; err != nil {
		tx.Rollback()
		return err
	}
	byID := make(map[string]portReservationRecord, len(existing))
	for _, record := range existing {
		byID[record.ID] = record
	}
	desired := make(map[string]bool, len(values))
	for _, value := range values {
		desired[value.ID] = true
		record := portRecordFromReservation(value)
		if current, ok := byID[value.ID]; ok {
			record.CreatedAt = current.CreatedAt
			if record.LeaseID == "" && current.LeaseID != "" {
				record.LeaseID, record.OwnerID = current.LeaseID, current.OwnerID
				record.ExpiresAt, record.ActivatedAt = current.ExpiresAt, current.ActivatedAt
			}
		} else {
			record.CreatedAt = now
		}
		record.UpdatedAt = now
		if err := tx.Table(s.portReservationsTable).Save(&record).Error; err != nil {
			tx.Rollback()
			return err
		}
	}
	for _, record := range existing {
		if desired[record.ID] || record.State == string(ReservationReleased) {
			continue
		}
		if record.LeaseID != "" && record.State == string(ReservationPlanned) && record.ExpiresAt != nil && record.ExpiresAt.After(now) {
			continue
		}
		updates := map[string]interface{}{"state": string(ReservationReleasing), "updated_at": now}
		if record.State == string(ReservationReleasing) {
			updates["state"], updates["released_at"] = string(ReservationReleased), now
		}
		if err := tx.Table(s.portReservationsTable).Where("id = ?", record.ID).Updates(updates).Error; err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit().Error
}

func (s *Store) EnsureCPUAllocations(values []CPUAllocation) error {
	activeIDs := make([]string, 0, len(values))
	for _, value := range values {
		activeIDs = append(activeIDs, value.ID)
		var existing cpuAllocationRecord
		result := s.db.Table(s.cpuAllocationsTable).Where("id = ?", value.ID).First(&existing)
		if gorm.IsRecordNotFoundError(result.Error) {
			if _, err := s.SaveCPUAllocation(value); err != nil {
				return err
			}
			continue
		}
		if result.Error != nil {
			return result.Error
		}
		if existing.EnvironmentID != value.EnvironmentID || existing.TargetID != value.TargetID {
			value.Policy, value.LogicalCPUIds, value.PhysicalCoreKeys = CPUPolicyNone, []int{}, []string{}
			value.AllowSMTSiblingRisk = false
			value.Warnings = []string{"Placement 已变化，旧 CPU 分配已重置为不绑核"}
			if _, err := s.SaveCPUAllocation(value); err != nil {
				return err
			}
		}
	}
	query := s.db.Table(s.cpuAllocationsTable)
	if len(activeIDs) == 0 {
		return query.Where("1 = 1").Delete(&cpuAllocationRecord{}).Error
	}
	return query.Where("id NOT IN (?)", activeIDs).Delete(&cpuAllocationRecord{}).Error
}

func (s *Store) SaveCPUAllocation(value CPUAllocation) (CPUAllocation, error) {
	now := s.now().UTC()
	record := cpuRecordFromAllocation(value)
	var existing cpuAllocationRecord
	result := s.db.Table(s.cpuAllocationsTable).Where("id = ?", value.ID).First(&existing)
	if gorm.IsRecordNotFoundError(result.Error) {
		record.CreatedAt, record.UpdatedAt = now, now
		if err := s.db.Table(s.cpuAllocationsTable).Create(&record).Error; err != nil {
			return CPUAllocation{}, err
		}
	} else if result.Error != nil {
		return CPUAllocation{}, result.Error
	} else if err := s.db.Table(s.cpuAllocationsTable).Where("id = ?", value.ID).Updates(map[string]interface{}{
		"environment_id": record.EnvironmentID, "target_id": record.TargetID, "room_id": record.RoomID, "world_id": record.WorldID,
		"policy": record.Policy, "logical_cpu_ids": record.LogicalCPUIds, "physical_core_keys": record.PhysicalCoreKeys,
		"allow_smt_sibling_risk": record.AllowSMTSiblingRisk, "warnings": record.Warnings, "updated_at": now,
	}).Error; err != nil {
		return CPUAllocation{}, err
	}
	record.UpdatedAt = now
	return allocationFromRecord(record), nil
}

func (s *Store) UpdateNetworkProfile(id string, input NetworkProfileUpdate) (NetworkProfile, error) {
	now := s.now().UTC()
	result := s.db.Table(s.networkProfilesTable).Where("id = ?", id).Updates(map[string]interface{}{
		"name": input.Name, "bind_address": input.BindAddress, "advertise_address": input.AdvertiseAddress, "updated_at": now,
	})
	if result.Error != nil {
		return NetworkProfile{}, result.Error
	}
	if result.RowsAffected == 0 {
		return NetworkProfile{}, ErrResourceNotFound
	}
	var record networkProfileRecord
	if err := s.db.Table(s.networkProfilesTable).Where("id = ?", id).First(&record).Error; err != nil {
		return NetworkProfile{}, err
	}
	return NetworkProfile{ID: record.ID, EnvironmentID: record.EnvironmentID, Name: record.Name, Mode: NetworkMode(record.Mode), ScopeID: record.ScopeID, BindAddress: record.BindAddress, AdvertiseAddress: record.AdvertiseAddress, UpdatedAt: record.UpdatedAt.UTC()}, nil
}

func reservationFromRecord(record portReservationRecord) PortReservation {
	return PortReservation{ID: record.ID, EnvironmentID: record.EnvironmentID, NetworkProfileID: record.NetworkProfileID, ScopeID: record.ScopeID, TargetID: record.TargetID, RoomID: record.RoomID, WorldID: record.WorldID, Cluster: record.Cluster, Shard: record.Shard, Purpose: PortPurpose(record.Purpose), Protocol: record.Protocol, BindAddress: record.BindAddress, Port: record.Port, State: ReservationState(record.State), Managed: record.Managed, LeaseID: record.LeaseID, OwnerID: record.OwnerID, ExpiresAt: utcTimePointer(record.ExpiresAt), ActivatedAt: utcTimePointer(record.ActivatedAt), ReleasedAt: utcTimePointer(record.ReleasedAt), UpdatedAt: record.UpdatedAt.UTC()}
}

func portRecordFromReservation(value PortReservation) portReservationRecord {
	return portReservationRecord{ID: value.ID, EnvironmentID: value.EnvironmentID, NetworkProfileID: value.NetworkProfileID, ScopeID: value.ScopeID, TargetID: value.TargetID, RoomID: value.RoomID, WorldID: value.WorldID, Cluster: value.Cluster, Shard: value.Shard, Purpose: string(value.Purpose), Protocol: value.Protocol, BindAddress: value.BindAddress, Port: value.Port, State: string(value.State), Managed: value.Managed, LeaseID: value.LeaseID, OwnerID: value.OwnerID, ExpiresAt: value.ExpiresAt, ActivatedAt: value.ActivatedAt, ReleasedAt: value.ReleasedAt}
}

func utcTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := value.UTC()
	return &result
}

func allocationFromRecord(record cpuAllocationRecord) CPUAllocation {
	logical := []int{}
	cores, warnings := []string{}, []string{}
	_ = json.Unmarshal([]byte(record.LogicalCPUIds), &logical)
	_ = json.Unmarshal([]byte(record.PhysicalCoreKeys), &cores)
	_ = json.Unmarshal([]byte(record.Warnings), &warnings)
	return CPUAllocation{ID: record.ID, EnvironmentID: record.EnvironmentID, TargetID: record.TargetID, RoomID: record.RoomID, WorldID: record.WorldID, Policy: CPUPolicy(record.Policy), LogicalCPUIds: logical, PhysicalCoreKeys: cores, AllowSMTSiblingRisk: record.AllowSMTSiblingRisk, Warnings: warnings, UpdatedAt: record.UpdatedAt.UTC()}
}

func cpuRecordFromAllocation(value CPUAllocation) cpuAllocationRecord {
	logical, _ := json.Marshal(value.LogicalCPUIds)
	cores, _ := json.Marshal(value.PhysicalCoreKeys)
	warnings, _ := json.Marshal(value.Warnings)
	return cpuAllocationRecord{ID: value.ID, EnvironmentID: value.EnvironmentID, TargetID: value.TargetID, RoomID: value.RoomID, WorldID: value.WorldID, Policy: string(value.Policy), LogicalCPUIds: string(logical), PhysicalCoreKeys: string(cores), AllowSMTSiblingRisk: value.AllowSMTSiblingRisk, Warnings: string(warnings)}
}

func sortedUniqueInts(values []int) []int {
	seen := make(map[int]bool, len(values))
	result := make([]int, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Ints(result)
	return result
}

func resourceStoreError(label string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", label, err)
}
