package playeractivity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"dont/internal/dstruntime"
	"dont/internal/runtimefiles"
	"dont/shared"
)

func (s *Service) readManifest(ctx context.Context, room, world string) (Manifest, error) {
	bundle, err := s.source.ReadActivityArtifacts(ctx, room, world, nil)
	if err != nil {
		return Manifest{}, err
	}
	if err = runtimefiles.ValidateArtifactBundle(shared.ArtifactPlayerActivity, bundle); err != nil {
		return Manifest{}, &activityReadError{code: "ACTIVITY_INVALID_MANIFEST_BUNDLE"}
	}
	for _, a := range bundle.Artifacts {
		if a.Name == "activity-manifest.json" {
			return manifestFromBundleData(a.Data)
		}
	}
	return Manifest{}, os.ErrNotExist
}
func (s *Service) applyConfig(ctx context.Context, room, world, scope string, m Manifest, c Settings, row *cursorRecord, now time.Time) {
	if m.Config.Revision == c.Revision {
		row.ConfigError = ""
		row.ConfigRetryAt = nil
		return
	}
	if s.commander == nil {
		row.ConfigError = "ACTIVITY_CONFIG_UNAVAILABLE"
		return
	}
	if row.ConfigRetryAt != nil && now.Before(*row.ConfigRetryAt) {
		return
	}
	retry := now.Add(30 * time.Second)
	row.ConfigRetryAt = &retry
	raw, _ := json.Marshal(c)
	arguments := map[string]any{}
	_ = json.Unmarshal(raw, &arguments)
	id := "activity-" + identity(room, world, scope, m.ProducerInstanceID, c.Revision, strconv.FormatInt(now.Unix()/30, 10))[:48]
	// Configuration gets a budget before history reads, not an expired finally context.
	commandCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	receipt, err := s.commander.ExecuteCommand(commandCtx, room, world, dstruntime.CommandRequest{RequestID: id, Action: "activity.configure", Arguments: arguments})
	row.ConfigError = ""
	if receipt.Code == "ACTIVITY_BUSY" {
		row.ConfigError = "ACTIVITY_BUSY"
	} else if err != nil || !receipt.OK {
		row.ConfigError = "ACTIVITY_CONFIG_APPLY_FAILED"
	}
	// Acceptance is not application; only the next trusted manifest can acknowledge it.
}
func (s *Service) collect(parent context.Context, room, world string, c Settings) time.Duration {
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()
	key := room + "/" + world
	s.mu.Lock()
	if s.active[key] {
		s.mu.Unlock()
		return 5 * time.Second
	}
	s.active[key] = true
	retryNow := s.retryNow[key]
	delete(s.retryNow, key)
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.active, key); s.mu.Unlock() }()
	row, err := s.store.cursor(room, world)
	if err != nil {
		return time.Minute
	}
	now := s.store.now().UTC()
	row.LastAttemptAt = &now
	fail := func(code string) time.Duration {
		if s.room(room) != nil {
			return time.Minute
		}
		row.State = "unavailable"
		row.SyncState = "retrying"
		row.Error = code
		at := now.Add(time.Minute)
		row.NextRetryAt = &at
		_ = s.store.saveCursor(s.store.db, row)
		return time.Minute
	}
	initialCtx, initialCancel := context.WithTimeout(ctx, 3*time.Second)
	scope, err := s.source.ActivityScope(initialCtx, room, world)
	if err != nil {
		initialCancel()
		return fail("NODE_UNAVAILABLE")
	}
	m, err := s.readManifest(initialCtx, room, world)
	initialCancel()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fail("ACTIVITY_RUNTIME_UNAVAILABLE")
		}
		return fail(manifestErrorCode(err))
	}
	checkScope, err := s.source.ActivityScope(ctx, room, world)
	if err != nil || scope != checkScope {
		return fail("ACTIVITY_PLACEMENT_CHANGED")
	}
	boundary := row.Scope != scope || row.Generation > m.Generation || row.StreamID == ""
	if boundary {
		if row.StreamID == "" && row.GapCount > 0 {
			row.LegacyCoverageBefore = &now
		}
		row.StreamID = identity(scope, m.ProducerInstanceID, strconv.FormatInt(m.Generation, 10), now.Format(time.RFC3339Nano))
		row.Generation = 0
		row.VerifiedGeneration = 0
		row.Scope = scope
		row.Dropped = 0
		row.RangeID = ""
		row.LastSequence = 0
		row.ConfigRetryAt = nil
	}
	s.applyConfig(ctx, room, world, scope, m, c, &row, now)
	events := []Event{}
	changes := map[string]recoveryRecord{}
	pending, err := s.store.pendingRecovery(room, world)
	if err != nil {
		return time.Minute
	}
	systemEvent := func(kind, code, stable string, fields map[string]any) {
		if fields == nil {
			fields = map[string]any{}
		}
		fields["code"] = code
		events = append(events, Event{ID: identity(room, world, row.StreamID, kind, code, stable), RoomID: room, WorldID: world, Kind: kind, ProducerInstanceID: m.ProducerInstanceID, SessionID: m.SessionID, OccurredAt: now, OccurredAtUnix: now.Unix(), ReceivedAt: now, Fields: fields})
	}
	if boundary || row.Producer != m.ProducerInstanceID {
		systemEvent("collection.instance", "RUNTIME_INSTANCE_CHANGED", m.ProducerInstanceID, nil)
		row.Producer = m.ProducerInstanceID
		row.Dropped = 0
		row.RangeID = ""
	}
	if m.Dropped > row.Dropped {
		systemEvent("collection.gap", "NODE_EVENTS_DROPPED", fmt.Sprint(m.Dropped), map[string]any{"dropped": m.Dropped - row.Dropped})
		row.GapCount++
		id := identity(row.StreamID, "dropped", m.ProducerInstanceID, fmt.Sprint(m.Dropped))
		changes[id] = recoveryRecord{ID: id, RoomID: room, WorldID: world, StreamID: row.StreamID, Scope: scope, Generation: m.Generation, EndGeneration: m.Generation, Producer: m.ProducerInstanceID, Session: m.SessionID, Shard: m.ShardID, State: "expired", Code: "NODE_EVENTS_DROPPED", FirstSeenAt: now, LastSeenAt: now}
		row.Dropped = m.Dropped
		row.RangeID = ""
	}
	remember := func(slot Slot, check segmentCheck, data []byte) {
		id := descriptorID(row, slot)
		r, ok := changes[id]
		if !ok {
			for _, p := range pending {
				if p.ID == id {
					r = p
					ok = true
					break
				}
			}
		}
		if !ok {
			r = recoveryRecord{ID: id, RoomID: room, WorldID: world, Scope: scope, StreamID: row.StreamID, Generation: slot.Generation, EndGeneration: slot.Generation, Slot: slot.Slot, Producer: slot.ProducerInstanceID, Session: slot.SessionID, Shard: slot.ShardID, FirstSequence: slot.FirstSequence, LastSequence: slot.LastSequence, FirstSeenAt: now, State: "pending"}
			row.GapCount++
			systemEvent("collection.gap", "SEGMENT_PENDING", id, map[string]any{"generation": slot.Generation})
		}
		fp := contentFingerprint(data)
		if !r.LastSeenAt.Equal(now) {
			if r.Fingerprint == fp {
				r.Observations++
			} else {
				r.Observations = 1
			}
			r.LastSeenAt = now
		}
		r.Fingerprint = fp
		r.Code = check.Code
		r.Field = check.Field
		r.TrustedEnvelope = check.TrustedEnvelope
		if check.TrustedEnvelope {
			raw, _ := json.Marshal(check.InvalidSequences)
			r.MissingSequences = string(raw)
		}
		r.RetryAt = now.Add(30 * time.Second)
		// Only a trusted complete envelope can isolate individual bad events. A
		// stable hash of malformed/unpublished bytes never proves corruption.
		r.State = "pending"
		changes[id] = r
	}
	budget := 8
	failureCode := ""
	fatal := false
	validLater := row.VerifiedGeneration
	for attempt := 0; attempt < 3 && budget > 0; attempt++ {
		slots := append([]Slot{}, m.Slots...)
		sort.Slice(slots, func(i, j int) bool { return slots[i].Generation < slots[j].Generation })
		byID := map[string]Slot{}
		for _, slot := range slots {
			byID[descriptorID(row, slot)] = slot
		}
		for _, p := range pending {
			if _, ok := byID[p.ID]; !ok || p.StreamID != row.StreamID || p.Scope != scope {
				p.State = "expired"
				p.Code = "ACTIVITY_OUTSIDE_SOURCE_WINDOW"
				p.LastSeenAt = now
				changes[p.ID] = p
			}
		}
		for id, p := range changes {
			if p.State == "pending" || p.State == "isolated" {
				if _, ok := byID[id]; !ok {
					p.State = "expired"
					p.Code = "ACTIVITY_OUTSIDE_SOURCE_WINDOW"
					changes[id] = p
				}
			}
		}
		if len(slots) > 0 && slots[0].Generation > row.Generation+1 {
			first, last := row.Generation+1, slots[0].Generation-1
			id := identity(row.StreamID, "window", fmt.Sprint(first), fmt.Sprint(last))
			changes[id] = recoveryRecord{ID: id, RoomID: room, WorldID: world, StreamID: row.StreamID, Scope: scope, Generation: first, EndGeneration: last, Code: "SPOOL_WINDOW_EXCEEDED", State: "expired", FirstSeenAt: now, LastSeenAt: now}
			systemEvent("collection.gap", "SPOOL_WINDOW_EXCEEDED", id, map[string]any{"missingSegments": last - first + 1})
			row.GapCount++
			row.Generation = last
			row.RangeID = ""
		}
		selected := []Slot{}
		// At most one deferred segment per batch, within the same eight-read budget.
		if attempt == 0 {
			for _, p := range pending {
				if slot, ok := byID[p.ID]; ok && p.Generation <= row.Generation && (retryNow || !now.Before(p.RetryAt)) {
					selected = append(selected, slot)
					break
				}
			}
		}
		for _, slot := range slots {
			if slot.Generation > row.Generation && len(selected) < budget {
				selected = append(selected, slot)
			}
		}
		if len(selected) == 0 {
			break
		}
		names := []string{}
		for _, slot := range selected {
			names = append(names, fmt.Sprintf("activity-%02d.json", slot.Slot))
		}
		budget -= len(selected)
		bundle, readErr := s.source.ReadActivityArtifacts(ctx, room, world, names)
		if readErr != nil {
			if errors.Is(readErr, runtimefiles.ErrArtifactChanging) && attempt < 2 && budget > 0 {
				fresh, e := s.readManifest(ctx, room, world)
				if e == nil && fresh.Generation >= m.Generation {
					m = fresh
					continue
				}
			}
			failureCode = "ACTIVITY_SEGMENT_READ_FAILED"
			break
		}
		if runtimefiles.ValidateArtifactBundle(shared.ArtifactPlayerActivity, bundle) != nil {
			failureCode = "ACTIVITY_INVALID_SEGMENT_BUNDLE"
			break
		}
		files := map[string][]byte{}
		for _, a := range bundle.Artifacts {
			files[a.Name] = a.Data
		}
		changed := false
		for _, slot := range selected {
			data := files[fmt.Sprintf("activity-%02d.json", slot.Slot)]
			check := inspectSegment(data, slot, now)
			// A deferred partial segment retries exactly its unresolved record IDs.
			// Previously committed events are neither rewritten nor treated as new gaps.
			for _, p := range pending {
				if matchesDescriptor(p, row, slot) && p.MissingSequences != "" && p.MissingSequences != "null" && check.TrustedEnvelope && !check.Fatal {
					var sequences []int64
					_ = json.Unmarshal([]byte(p.MissingSequences), &sequences)
					wanted := map[int64]bool{}
					for _, seq := range sequences {
						wanted[seq] = true
					}
					kept := []Event{}
					for _, e := range check.Events {
						if wanted[e.Sequence] {
							kept = append(kept, e)
						}
					}
					check.Events = kept
					bad := []int64{}
					for _, seq := range check.InvalidSequences {
						if wanted[seq] {
							bad = append(bad, seq)
						}
					}
					check.InvalidSequences = bad
					if len(bad) == 0 {
						check.Code = ""
						check.Field = ""
					}
					break
				}
			}

			if check.Code != "" {
				remember(slot, check, data)
				failureCode = check.Code
				row.RangeID = ""
				changed = true
			}
			if check.Fatal {
				fatal = true
				break
			}
			// Strictly valid records in a complete trusted envelope can be retained
			// even while independent invalid records remain explicitly unconfirmed.
			deferred := slot.Generation <= row.Generation
			rangeID := row.RangeID
			if deferred || rangeID == "" || row.LastProducer != slot.ProducerInstanceID || row.LastSession != slot.SessionID || row.LastSequence+1 != slot.FirstSequence {
				rangeID = identity(row.StreamID, slot.ProducerInstanceID, slot.SessionID, slot.ShardID, fmt.Sprint(slot.Generation), "range")
			}
			previous := slot.FirstSequence - 1
			for _, event := range check.Events {
				if event.Sequence != previous+1 {
					rangeID = identity(rangeID, fmt.Sprint(event.Sequence), "boundary")
				}
				event.RoomID = room
				event.WorldID = world
				event.ProducerInstanceID = slot.ProducerInstanceID
				event.SessionID = slot.SessionID
				event.ID = identity(room, world, scope, slot.ProducerInstanceID, strconv.FormatInt(event.Sequence, 10))
				event.ContinuousRangeID = rangeID
				event.OccurredAt = time.Unix(event.OccurredAtUnix, 0).UTC()
				event.ReceivedAt = now
				events = append(events, event)
				previous = event.Sequence
				if row.LastEventAt == nil || event.OccurredAt.After(*row.LastEventAt) {
					at := event.OccurredAt
					row.LastEventAt = &at
				}
			}
			id := descriptorID(row, slot)
			if check.Code == "" {
				if slot.Generation > validLater {
					validLater = slot.Generation
				}
				for _, p := range pending {
					if p.ID == id {
						p.State = "resolved"
						cutoff := now.Add(-time.Duration(c.RetentionDays) * 24 * time.Hour)
						if row.DiscardBefore != nil && row.DiscardBefore.After(cutoff) {
							cutoff = *row.DiscardBefore
						}
						admitted := len(check.Events) > 0
						for _, e := range check.Events {
							if !time.Unix(e.OccurredAtUnix, 0).After(cutoff) {
								admitted = false
							}
						}
						if !admitted {
							p.State = "expired"
							p.Code = "ACTIVITY_RETENTION_EXCEEDED"
						}
						p.LastSeenAt = now
						changes[id] = p
					}
				}
				if r, ok := changes[id]; ok && r.State != "expired" {
					r.State = "resolved"
					changes[id] = r
				}
			}
			if !deferred {
				// This is scan progress, not a completeness watermark. Every unconfirmed
				// descriptor advances transactionally with its recovery evidence.
				row.Generation = slot.Generation
				row.RangeID = rangeID
				row.LastProducer = slot.ProducerInstanceID
				row.LastSession = slot.SessionID
				row.LastSequence = slot.LastSequence
				if check.Code != "" {
					row.RangeID = ""
				}
			}
		}
		if fatal || !changed || budget == 0 {
			break
		}
		// Rotation recovery only; normal reads remain one manifest plus one batch.
		fresh, e := s.readManifest(ctx, room, world)
		if e != nil {
			break
		}
		if fresh.Generation < m.Generation {
			failureCode = "ACTIVITY_STREAM_CHANGED"
			break
		}
		m = fresh
		// A failed descriptor already recorded for this observation is retried in a
		// later poll; refresh only selects newly published, independently valid work.
	}
	for id, r := range changes {
		if r.State == "pending" && r.TrustedEnvelope && r.Observations >= 2 && validLater > r.Generation {
			r.State = "isolated"
			changes[id] = r
		}
	}
	after, err := s.source.ActivityScope(ctx, room, world)
	if err != nil || after != scope || s.room(room) != nil {
		return 5 * time.Second
	}
	sourceAt := time.Unix(m.UpdatedAtUnix, 0).UTC()
	row.SourceUpdatedAt = &sourceAt
	row.AppliedRevision = m.Config.Revision
	if row.AppliedRevision == c.Revision {
		row.ConfigError = ""
		row.ConfigRetryAt = nil
	}
	row.SourceSession = m.SessionID
	row.SourceGeneration = m.Generation
	row.VerifiedGeneration = validLater
	row.NodePending = m.Pending
	row.PublicationPhase = m.PublicationPhase
	row.WriteUnconfirmed = m.WriteUnconfirmed
	// A frozen manifest cannot describe an I/O that has not completed. Consult
	// existing health only on failures/stale output, never add steady-state reads.
	if failureCode != "" || row.ConfigError == "ACTIVITY_BUSY" || m.PlayerCount > 0 && now.Sub(sourceAt) > 2*time.Minute {
		s.readPublicationHealth(ctx, room, world, m, &row, now)
	}

	if failureCode == "" || len(events) > 0 {
		row.LastCollectedAt = &now
	}
	row.Error = failureCode
	row.SyncState = "caught_up"
	row.NextRetryAt = nil
	row.State = "recording"
	if !m.Config.Enabled {
		row.State = "disabled"
	} else if m.PlayerCount == 0 {
		row.State = "idle"
	}
	if m.LastError != "" {
		row.State = "degraded"
		if row.Error == "" {
			row.Error = "NODE_ACTIVITY_ERROR"
		}
	}
	if now.Sub(sourceAt) > 2*time.Minute && row.State == "recording" {
		row.State = "inactive"
	}
	delay := time.Duration(c.SyncIntervalSeconds) * time.Second
	if failureCode != "" {
		row.SyncState = "retrying"
		delay = 30 * time.Second
		if row.Generation < m.Generation && !fatal {
			delay = 5 * time.Second
		}
	} else if row.Generation < m.Generation {
		row.SyncState = "catching_up"
		delay = 5 * time.Second
	} else if !c.Enabled && row.AppliedRevision == c.Revision {
		delay = time.Minute
	} else if m.PlayerCount == 0 && row.AppliedRevision == c.Revision {
		delay = 30 * time.Second
	}
	recovery := []recoveryRecord{}
	outstanding := false
	for _, p := range pending {
		r := p
		if v, ok := changes[p.ID]; ok {
			r = v
		}
		if r.State == "pending" || r.State == "isolated" {
			outstanding = true
		}
	}
	for _, r := range changes {
		recovery = append(recovery, r)
		if r.State == "pending" || r.State == "isolated" {
			outstanding = true
		}
	}
	if outstanding && row.SyncState == "caught_up" {
		row.SyncState = "incomplete"
	}
	if failureCode != "" || outstanding {
		retry := now.Add(delay)
		row.NextRetryAt = &retry
	}
	after, err = s.source.ActivityScope(ctx, room, world)
	if err != nil || after != scope || s.room(room) != nil {
		return 5 * time.Second
	}
	// Re-read policy after I/O so a retention reduction cannot backfill old data.
	current, err := s.store.Settings(room)
	if err != nil {
		return time.Minute
	}
	// The transaction applies the latest policy and quota floor, while it
	// still has the original events needed to classify incomplete recovery.
	if err = s.store.ingestRecovery(row, events, recovery); err != nil {
		return time.Minute
	}
	s.maybePrune(room, current, len(events))
	return delay
}

func (s *Service) readPublicationHealth(ctx context.Context, room, world string, m Manifest, row *cursorRecord, now time.Time) {
	source, ok := s.source.(interface {
		ReadArtifacts(context.Context, string, string, shared.ArtifactKind) (shared.RuntimeArtifactBundle, error)
	})
	if !ok {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	bundle, err := source.ReadArtifacts(readCtx, room, world, shared.ArtifactRuntimeHealth)
	if err != nil || runtimefiles.ValidateArtifactBundle(shared.ArtifactRuntimeHealth, bundle) != nil {
		return
	}
	for _, a := range bundle.Artifacts {
		if a.Name != "health.json" {
			continue
		}
		var h dstruntime.Health
		if decode(a.Data, &h) != nil || !h.FreshAt(now) || h.SessionID != m.SessionID || h.ShardID != m.ShardID {
			return
		}
		activity, exists := h.Modules["activity"]
		if !exists || activity.ProducerInstanceID != m.ProducerInstanceID {
			return
		}
		row.PublicationPhase = activity.PublicationPhase
		row.WriteUnconfirmed = activity.WriteUnconfirmed
	}
}
