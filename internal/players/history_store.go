package players

import (
	"time"

	"github.com/jinzhu/gorm"
)

func copyFieldStates(fields FieldStates) FieldStates {
	result := make(FieldStates, len(fields))
	for key, value := range fields {
		result[key] = value
	}
	return result
}

func storedWorldConfirmed(record playerRecord) bool {
	if record.WorldID == "" {
		return false
	}
	if state, exists := decodeFieldStates(record.FieldStates)["world"]; exists {
		return state.Status != FreshnessUnavailable && state.ObservedAt != nil
	}
	// Compatibility for records collected before world evidence was explicit.
	if record.GameplayState == GameplayStateSelectingCharacter || record.GameplayState == GameplayStateLoading {
		return false
	}
	return candidateHasLocalPlayer(snapshotCandidate{observation: Observation{
		GameplayState: record.GameplayState, HealthPercent: record.HealthPercent,
		HungerPercent: record.HungerPercent, SanityPercent: record.SanityPercent,
		Temperature: record.Temperature, Moisture: record.Moisture,
	}})
}

func storedWorldField(record playerRecord) FieldState {
	if state, exists := decodeFieldStates(record.FieldStates)["world"]; exists {
		return state
	}
	return FieldState{Source: SourceRuntime, ObservedAt: utcPointer(&record.LastSeenAt), Status: FreshnessStale}
}

func (s *Store) enrichHistoryPlayer(tx *gorm.DB, existing playerRecord, worldID, worldName string, observation Observation, importedAt time.Time) error {
	fields := decodeFieldStates(existing.FieldStates)
	legacyImport := len(fields) == 0 && !existing.Online && existing.FirstSeenAt.Equal(existing.LastSeenAt) && existing.LastSeenAt.Equal(existing.LastRefreshedAt)
	updates := map[string]interface{}{}
	if !observation.FirstSeenAt.IsZero() && (existing.FirstSeenAt.IsZero() || observation.FirstSeenAt.Before(existing.FirstSeenAt)) {
		updates["first_seen_at"] = observation.FirstSeenAt
		fields["firstSeenAt"] = historicalField(observation.FirstSeenAt)
	}
	if !observation.LastSeenAt.IsZero() {
		// Old history-only records used the import time for all four clocks.
		// Replace that fabricated timestamp only when no field has evidence.
		if observation.LastSeenAt.After(existing.LastSeenAt) || observation.LastSeenAt.Equal(existing.LastSeenAt) && fields["lastSeenAt"].Source == "" || legacyImport {
			updates["last_seen_at"] = observation.LastSeenAt
			fields["lastSeenAt"] = historicalField(observation.LastSeenAt)
		}
		worldState := storedWorldField(existing)
		if !storedWorldConfirmed(existing) || worldState.ObservedAt == nil || observation.LastSeenAt.After(*worldState.ObservedAt) {
			updates["world_id"], updates["world_name"] = worldID, worldName
			fields["world"] = historicalField(observation.LastSeenAt)
			updates["presence_conflict"] = false
			updates["observed_world_ids"] = encodeWorldIDs([]string{worldID})
		}
		nameState := historyFieldWithFallback(fields["name"], existing.LastSeenAt)
		if observation.Name != "" && observation.Name != observation.ID && (existing.Name == "" || existing.Name == observation.ID || newerHistoryField(nameState, observation.LastSeenAt)) {
			updates["name"] = observation.Name
			fields["name"] = historicalField(observation.LastSeenAt)
		}
	}
	if observation.Prefab != "" {
		incoming := observation.Fields["prefab"]
		if existing.Prefab == "" || incoming.ObservedAt != nil && newerHistoryField(historyFieldWithFallback(fields["prefab"], existing.LastSeenAt), *incoming.ObservedAt) {
			updates["prefab"] = observation.Prefab
			if incoming.ObservedAt != nil {
				fields["prefab"] = incoming
			}
		}
	}
	if existing.NetID == "" && observation.NetID != "" {
		updates["net_id"] = observation.NetID
	}
	for field, at := range map[string]time.Time{"connected": observation.LastConnectedAt, "disconnected": observation.LastDisconnectedAt} {
		if !at.IsZero() && newerHistoryField(fields[field], at) {
			fields[field] = historicalField(at)
		}
	}
	// A later real disconnect may settle a stale snapshot. An old disconnect
	// must never disconnect someone who has since rejoined another shard.
	if !observation.LastDisconnectedAt.IsZero() && observation.LastDisconnectedAt.After(existing.LastRefreshedAt) &&
		!observation.LastDisconnectedAt.Before(existing.LastSeenAt) && !observation.LastDisconnectedAt.Before(observation.LastSeenAt) {
		updates["online"] = false
		updates["status_changed_at"] = observation.LastDisconnectedAt
		updates["last_refreshed_at"] = observation.LastDisconnectedAt
		fields["online"] = historicalField(observation.LastDisconnectedAt)
		markFieldStale(fields, "gameplayState")
	}
	if len(updates) == 0 && len(fields) == 0 {
		return nil
	}
	encoded, err := encodeFieldStates(fields)
	if err != nil {
		return err
	}
	if len(updates) == 0 && encoded == existing.FieldStates {
		return nil
	}
	updates["field_states"] = encoded
	return tx.Table(s.table).Where("room_id = ? AND user_id = ?", existing.RoomID, existing.UserID).Updates(updates).Error
}

func historyFieldWithFallback(state FieldState, at time.Time) FieldState {
	if state.ObservedAt == nil && !at.IsZero() {
		state.ObservedAt = &at
	}
	return state
}

func newerHistoryField(state FieldState, at time.Time) bool {
	return state.ObservedAt == nil || at.After(*state.ObservedAt)
}
