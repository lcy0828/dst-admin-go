package topology

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
)

const defaultPortLeaseTTL = 15 * time.Minute

func (s *Service) ReservePorts(ctx context.Context, request PortAllocationRequest) (PortAllocation, error) {
	if err := ctx.Err(); err != nil {
		return PortAllocation{}, err
	}
	if _, _, err := s.resourceContext(ctx); err != nil {
		return PortAllocation{}, err
	}
	return s.store.ReservePorts(request)
}

func (s *Service) ActivatePorts(ctx context.Context, leaseID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.store.ActivatePorts(leaseID)
}

func (s *Service) ReleasePorts(ctx context.Context, leaseID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.store.BeginReleasePorts(leaseID); err != nil {
		return err
	}
	return s.store.CompleteReleasePorts(leaseID)
}

func (s *Store) ReservePorts(request PortAllocationRequest) (PortAllocation, error) {
	request.OwnerID, request.TargetID = strings.TrimSpace(request.OwnerID), strings.TrimSpace(request.TargetID)
	request.RoomID, request.Cluster = strings.TrimSpace(request.RoomID), strings.TrimSpace(request.Cluster)
	if request.OwnerID == "" || request.TargetID == "" || request.RoomID == "" || request.Cluster == "" || len(request.Requests) == 0 {
		return PortAllocation{}, ErrResourceNotFound
	}
	if request.TTL <= 0 {
		request.TTL = defaultPortLeaseTTL
	}
	if request.TTL > time.Hour {
		request.TTL = time.Hour
	}
	s.portMu.Lock()
	defer s.portMu.Unlock()

	tx := s.db.Begin()
	if tx.Error != nil {
		return PortAllocation{}, tx.Error
	}
	rollback := func(err error) (PortAllocation, error) { tx.Rollback(); return PortAllocation{}, err }
	var environment executionEnvironmentRecord
	if err := tx.Table(s.environmentsTable).Where("target_id = ?", request.TargetID).First(&environment).Error; err != nil {
		if gorm.IsRecordNotFoundError(err) {
			return rollback(ErrResourceNotFound)
		}
		return rollback(err)
	}
	var profile networkProfileRecord
	if err := tx.Table(s.networkProfilesTable).Where("id = ?", environment.NetworkProfileID).First(&profile).Error; err != nil {
		return rollback(err)
	}
	now := s.now().UTC()
	lock := portAllocationLockRecord{ScopeID: profile.ScopeID, UpdatedAt: now}
	result := tx.Table(s.portLocksTable).Where("scope_id = ?", profile.ScopeID).First(&lock)
	if gorm.IsRecordNotFoundError(result.Error) {
		if err := tx.Table(s.portLocksTable).Create(&lock).Error; err != nil {
			return rollback(err)
		}
	} else if result.Error != nil {
		return rollback(result.Error)
	}
	if err := tx.Table(s.portLocksTable).Where("scope_id = ?", profile.ScopeID).Updates(map[string]interface{}{"revision": gorm.Expr("revision + ?", 1), "updated_at": now}).Error; err != nil {
		return rollback(err)
	}
	if err := tx.Table(s.portReservationsTable).Where("state = ? AND expires_at IS NOT NULL AND expires_at <= ?", string(ReservationPlanned), now).Updates(map[string]interface{}{
		"state": string(ReservationReleased), "released_at": now, "updated_at": now,
	}).Error; err != nil {
		return rollback(err)
	}
	var records []portReservationRecord
	if err := tx.Table(s.portReservationsTable).Where("scope_id = ? AND protocol = ? AND state IN (?)", profile.ScopeID, "udp", []string{
		string(ReservationActive), string(ReservationPlanned), string(ReservationObserved), string(ReservationReleasing),
	}).Find(&records).Error; err != nil {
		return rollback(err)
	}
	requests := append([]PortRequest(nil), request.Requests...)
	sort.SliceStable(requests, func(i, j int) bool {
		if requests[i].Preferred != requests[j].Preferred {
			return requests[i].Preferred < requests[j].Preferred
		}
		return requests[i].Purpose < requests[j].Purpose
	})
	leaseID := uuid.NewString()
	expiresAt := now.Add(request.TTL)
	allocation := PortAllocation{LeaseID: leaseID, ExpiresAt: expiresAt, Reservations: make([]PortReservation, 0, len(requests))}
	for _, item := range requests {
		item.WorldID, item.Shard = strings.TrimSpace(item.WorldID), strings.TrimSpace(item.Shard)
		if item.WorldID == "" || item.Shard == "" || !validPortPurpose(item.Purpose) || item.Preferred < 1 || item.Preferred > 65535 {
			return rollback(ErrResourceConflict)
		}
		candidate := item.Preferred
		for candidate <= 65535 && reservationPortConflict(records, allocation.Reservations, request.RoomID, item, profile, candidate) {
			if item.Strict {
				return rollback(&ResourceConflictError{Preflight: ResourcePreflight{Ready: false, Conflicts: []ResourceConflict{{Code: "UDP_PORT_CONFLICT", ScopeID: profile.ScopeID, Port: candidate, TargetID: request.TargetID, RoomID: request.RoomID, WorldID: item.WorldID, Message: "请求的 UDP 端口已被当前网络作用域占用"}}}})
			}
			candidate++
		}
		if candidate > 65535 {
			return rollback(ErrResourceConflict)
		}
		reservation := PortReservation{
			ID:            stableResourceID("port-lease", leaseID+"\x00"+item.WorldID+"\x00"+string(item.Purpose)),
			EnvironmentID: environment.ID, NetworkProfileID: profile.ID, ScopeID: profile.ScopeID, TargetID: request.TargetID,
			RoomID: request.RoomID, WorldID: item.WorldID, Cluster: request.Cluster, Shard: item.Shard,
			Purpose: item.Purpose, Protocol: "udp", BindAddress: profile.BindAddress, Port: candidate,
			State: ReservationPlanned, Managed: true, LeaseID: leaseID, OwnerID: request.OwnerID, ExpiresAt: &expiresAt,
		}
		record := portRecordFromReservation(reservation)
		record.CreatedAt, record.UpdatedAt = now, now
		if err := tx.Table(s.portReservationsTable).Create(&record).Error; err != nil {
			return rollback(err)
		}
		allocation.Reservations = append(allocation.Reservations, reservation)
	}
	if err := tx.Commit().Error; err != nil {
		return PortAllocation{}, err
	}
	return allocation, nil
}

func (s *Store) ActivatePorts(leaseID string) error {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return ErrResourceNotFound
	}
	now := s.now().UTC()
	result := s.db.Table(s.portReservationsTable).Where("lease_id = ? AND state = ? AND (expires_at IS NULL OR expires_at > ?)", leaseID, string(ReservationPlanned), now).Updates(map[string]interface{}{
		"state": string(ReservationActive), "activated_at": now, "expires_at": nil, "updated_at": now,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		var count int
		if err := s.db.Table(s.portReservationsTable).Where("lease_id = ? AND state = ?", leaseID, string(ReservationActive)).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return ErrResourceNotFound
		}
	}
	return nil
}

func (s *Store) BeginReleasePorts(leaseID string) error {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return ErrResourceNotFound
	}
	now := s.now().UTC()
	return s.db.Table(s.portReservationsTable).Where("lease_id = ? AND state IN (?)", leaseID, []string{string(ReservationPlanned), string(ReservationActive)}).Updates(map[string]interface{}{
		"state": string(ReservationReleasing), "updated_at": now,
	}).Error
}

func (s *Store) CompleteReleasePorts(leaseID string) error {
	leaseID = strings.TrimSpace(leaseID)
	if leaseID == "" {
		return ErrResourceNotFound
	}
	now := s.now().UTC()
	return s.db.Table(s.portReservationsTable).Where("lease_id = ? AND state = ?", leaseID, string(ReservationReleasing)).Updates(map[string]interface{}{
		"state": string(ReservationReleased), "released_at": now, "updated_at": now,
	}).Error
}

func reservationPortConflict(records []portReservationRecord, allocated []PortReservation, roomID string, item PortRequest, profile networkProfileRecord, port int) bool {
	for _, record := range records {
		if record.Port != port || !bindAddressesConflict(record.BindAddress, profile.BindAddress) {
			continue
		}
		if record.RoomID == roomID && record.WorldID == item.WorldID && record.Purpose == string(item.Purpose) {
			continue
		}
		return true
	}
	for _, reservation := range allocated {
		if reservation.Port == port && bindAddressesConflict(reservation.BindAddress, profile.BindAddress) {
			return true
		}
	}
	return false
}

func validPortPurpose(value PortPurpose) bool {
	switch value {
	case PortDSTServer, PortClusterMaster, PortSteamAuth, PortSteamMasterServer:
		return true
	default:
		return false
	}
}
