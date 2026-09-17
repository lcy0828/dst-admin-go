package modpublication

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

type ReplicaStage string

const (
	ReplicaStageDesired  ReplicaStage = "desired"
	ReplicaStageCache    ReplicaStage = "cache"
	ReplicaStagePublish  ReplicaStage = "publish"
	ReplicaStageComplete ReplicaStage = "complete"
	ReplicaStageActivate ReplicaStage = "activate"
	ReplicaStageRollback ReplicaStage = "rollback"
	ReplicaStageObserve  ReplicaStage = "observe"
)

type InstallationReplicaObservation struct {
	Mods       map[string]string
	Worlds     map[string]WorldReplicaObservation
	ObservedAt time.Time
}

type WorldReplicaObservation struct {
	ConfigSHA256 string
	Mods         map[string]string
}

type ReplicaCoverage struct {
	ReadyTargets     int `json:"readyTargets"`
	TotalTargets     int `json:"totalTargets"`
	CachedTargets    int `json:"cachedTargets"`
	PublishedTargets int `json:"publishedTargets"`
	ConfiguredWorlds int `json:"configuredWorlds"`
	LoadedWorlds     int `json:"loadedWorlds"`
	TotalWorlds      int `json:"totalWorlds"`
}

type ReplicaWorldState struct {
	RoomID           string     `json:"roomId"`
	WorldID          string     `json:"worldId"`
	Configured       bool       `json:"configured"`
	Loaded           bool       `json:"loaded"`
	ConfigSHA256     string     `json:"configSha256"`
	ObservedRevision string     `json:"observedRevision,omitempty"`
	ObservedAt       *time.Time `json:"observedAt,omitempty"`
	ErrorStage       string     `json:"errorStage,omitempty"`
	ErrorMessage     string     `json:"errorMessage,omitempty"`
}

type ReplicaTargetState struct {
	TargetID       string                `json:"targetId"`
	InstallationID string                `json:"installationId"`
	TreeSHA256     string                `json:"treeSha256"`
	ManifestSHA256 string                `json:"manifestSha256,omitempty"`
	Size           int64                 `json:"size"`
	FileCount      int                   `json:"fileCount"`
	Cached         bool                  `json:"cached"`
	Published      bool                  `json:"published"`
	Ready          bool                  `json:"ready"`
	Worlds         []ReplicaWorldState   `json:"worlds"`
	ObservedAt     *time.Time            `json:"observedAt,omitempty"`
	ErrorStage     string                `json:"errorStage,omitempty"`
	ErrorMessage   string                `json:"errorMessage,omitempty"`
	FetchSource    string                `json:"fetchSource,omitempty"`
	FetchAttempts  []ReplicaFetchAttempt `json:"fetchAttempts,omitempty"`
}

type ReplicaFetchAttempt struct {
	Source         string     `json:"source"`
	Feasibility    string     `json:"feasibility"`
	Status         string     `json:"status"`
	Selected       bool       `json:"selected,omitempty"`
	Bytes          int64      `json:"bytes,omitempty"`
	DurationMillis int64      `json:"durationMs,omitempty"`
	BytesPerSecond int64      `json:"bytesPerSecond,omitempty"`
	ErrorCode      string     `json:"errorCode,omitempty"`
	ErrorMessage   string     `json:"errorMessage,omitempty"`
	ObservedAt     *time.Time `json:"observedAt,omitempty"`
}

type ReplicaModState struct {
	WorkshopID       string               `json:"workshopId"`
	ReadyTargets     int                  `json:"readyTargets"`
	TotalTargets     int                  `json:"totalTargets"`
	ConfiguredWorlds int                  `json:"configuredWorlds"`
	LoadedWorlds     int                  `json:"loadedWorlds"`
	TotalWorlds      int                  `json:"totalWorlds"`
	Targets          []ReplicaTargetState `json:"targets"`
}

type RoomReplicaState struct {
	RoomID          string            `json:"roomId"`
	DesiredRevision string            `json:"desiredRevision,omitempty"`
	Coverage        ReplicaCoverage   `json:"coverage"`
	Items           []ReplicaModState `json:"items"`
	UpdatedAt       *time.Time        `json:"updatedAt,omitempty"`
}

type CachedReplicaCandidate struct {
	TargetID       string
	InstallationID string
	ObservedAt     time.Time
}

type artifactReplicaRecord struct {
	ID                string `gorm:"primary_key;type:char(64)"`
	TargetID          string `gorm:"type:varchar(128);index;not null"`
	InstallationID    string `gorm:"type:varchar(128);index;not null"`
	WorkshopID        string `gorm:"type:varchar(32);index;not null"`
	TreeSHA256        string `gorm:"type:char(64);index;not null"`
	ManifestSHA256    string `gorm:"type:char(64)"`
	Size              int64  `gorm:"not null"`
	FileCount         int    `gorm:"not null"`
	DesiredRevision   string `gorm:"type:char(64);index"`
	Cached            bool   `gorm:"not null"`
	Published         bool   `gorm:"not null"`
	FetchSource       string `gorm:"type:varchar(32)"`
	FetchAttemptsJSON string `gorm:"type:text"`
	ObservedAt        *time.Time
	ErrorStage        string    `gorm:"type:varchar(32)"`
	ErrorMessage      string    `gorm:"type:text"`
	CreatedAt         time.Time `gorm:"not null"`
	UpdatedAt         time.Time `gorm:"index;not null"`
}

type worldReplicaRecord struct {
	ID               string `gorm:"primary_key;type:char(64)"`
	RoomID           string `gorm:"type:varchar(128);index;not null"`
	WorldID          string `gorm:"type:varchar(128);index;not null"`
	TargetID         string `gorm:"type:varchar(128);index;not null"`
	InstallationID   string `gorm:"type:varchar(128);index;not null"`
	WorkshopID       string `gorm:"type:varchar(32);index;not null"`
	TreeSHA256       string `gorm:"type:char(64);index;not null"`
	ConfigSHA256     string `gorm:"type:char(64);not null"`
	DesiredRevision  string `gorm:"type:char(64);index;not null"`
	ObservedRevision string `gorm:"type:char(64)"`
	Configured       bool   `gorm:"not null"`
	Loaded           bool   `gorm:"not null"`
	ObservedAt       *time.Time
	ErrorStage       string    `gorm:"type:varchar(32)"`
	ErrorMessage     string    `gorm:"type:text"`
	CreatedAt        time.Time `gorm:"not null"`
	UpdatedAt        time.Time `gorm:"index;not null"`
}

type ReplicaStore struct {
	db            *gorm.DB
	artifactTable string
	worldTable    string
	now           func() time.Time
}

func NewReplicaStore(db *gorm.DB, tablePrefix string) *ReplicaStore {
	prefix := strings.TrimSpace(tablePrefix)
	return &ReplicaStore{
		db: db, artifactTable: prefix + "mod_artifact_replica", worldTable: prefix + "mod_world_replica", now: time.Now,
	}
}

func (s *ReplicaStore) Migrate() error {
	if s == nil || s.db == nil {
		return errors.New("Mod replica database is required")
	}
	if err := s.db.Table(s.artifactTable).AutoMigrate(&artifactReplicaRecord{}).Error; err != nil {
		return fmt.Errorf("migrate Mod artifact replicas: %w", err)
	}
	if err := s.db.Table(s.worldTable).AutoMigrate(&worldReplicaRecord{}).Error; err != nil {
		return fmt.Errorf("migrate Mod world replicas: %w", err)
	}
	return nil
}

func (s *ReplicaStore) SetDesired(plan Plan) error {
	if s == nil || s.db == nil || validatePlan(plan) != nil {
		return ErrInvalidInput
	}
	now := s.now().UTC()
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	rollback := func(err error) error {
		tx.Rollback()
		return err
	}
	desiredWorldIDs := make(map[string]bool)
	for _, target := range plan.Targets {
		for _, artifact := range target.Mods {
			record := artifactReplicaRecord{
				ID:       artifactReplicaID(target.TargetID, target.InstallationID, artifact.WorkshopID, artifact.TreeSHA256),
				TargetID: target.TargetID, InstallationID: target.InstallationID, WorkshopID: artifact.WorkshopID,
				TreeSHA256: strings.ToLower(artifact.TreeSHA256), ManifestSHA256: strings.ToLower(artifact.ManifestSHA256),
				Size: artifact.Size, FileCount: artifact.FileCount, DesiredRevision: plan.PlanHash,
				CreatedAt: now, UpdatedAt: now,
			}
			if err := upsertDesiredArtifact(tx, s.artifactTable, record); err != nil {
				return rollback(err)
			}
		}
		for _, world := range target.Worlds {
			configSHA := hashBytes(world.ModOverrides)
			for _, artifact := range world.Mods {
				record := worldReplicaRecord{
					ID:     worldReplicaID(world.RoomID, world.WorldID, target.TargetID, target.InstallationID, artifact.WorkshopID, artifact.TreeSHA256, configSHA),
					RoomID: world.RoomID, WorldID: world.WorldID, TargetID: target.TargetID, InstallationID: target.InstallationID,
					WorkshopID: artifact.WorkshopID, TreeSHA256: strings.ToLower(artifact.TreeSHA256), ConfigSHA256: configSHA,
					DesiredRevision: plan.PlanHash, CreatedAt: now, UpdatedAt: now,
				}
				desiredWorldIDs[record.ID] = true
				if err := upsertDesiredWorld(tx, s.worldTable, record); err != nil {
					return rollback(err)
				}
			}
		}
	}
	var existing []worldReplicaRecord
	if err := tx.Table(s.worldTable).Where("room_id = ?", plan.RoomID).Find(&existing).Error; err != nil {
		return rollback(err)
	}
	for _, record := range existing {
		if !desiredWorldIDs[record.ID] {
			if err := tx.Table(s.worldTable).Where("id = ?", record.ID).Delete(&worldReplicaRecord{}).Error; err != nil {
				return rollback(err)
			}
		}
	}
	return tx.Commit().Error
}

// EnsureArtifactDesired makes cache tracking safe for installation-scoped
// content updates that do not have a room publication plan.
func (s *ReplicaStore) EnsureArtifactDesired(target TargetPlan, artifact ContentArtifact, revision string) error {
	if s == nil || s.db == nil || !validID(target.TargetID) || !validID(target.InstallationID) ||
		validateStoredArtifact(artifact) != nil || !validSHA(revision) {
		return ErrInvalidInput
	}
	now := s.now().UTC()
	desired := artifactReplicaRecord{
		ID:       artifactReplicaID(target.TargetID, target.InstallationID, artifact.WorkshopID, artifact.TreeSHA256),
		TargetID: target.TargetID, InstallationID: target.InstallationID, WorkshopID: artifact.WorkshopID,
		TreeSHA256: strings.ToLower(artifact.TreeSHA256), ManifestSHA256: strings.ToLower(artifact.ManifestSHA256),
		Size: artifact.Size, FileCount: artifact.FileCount, DesiredRevision: strings.ToLower(revision),
		CreatedAt: now, UpdatedAt: now,
	}
	return upsertDesiredArtifact(s.db, s.artifactTable, desired)
}

func upsertDesiredArtifact(tx *gorm.DB, table string, desired artifactReplicaRecord) error {
	var current artifactReplicaRecord
	result := tx.Table(table).Where("id = ?", desired.ID).First(&current)
	if gorm.IsRecordNotFoundError(result.Error) {
		return tx.Table(table).Create(&desired).Error
	}
	if result.Error != nil {
		return result.Error
	}
	return tx.Table(table).Where("id = ?", desired.ID).Updates(map[string]interface{}{
		"manifest_sha256": desired.ManifestSHA256, "size": desired.Size, "file_count": desired.FileCount,
		"desired_revision": desired.DesiredRevision, "updated_at": desired.UpdatedAt,
	}).Error
}

func upsertDesiredWorld(tx *gorm.DB, table string, desired worldReplicaRecord) error {
	var current worldReplicaRecord
	result := tx.Table(table).Where("id = ?", desired.ID).First(&current)
	if gorm.IsRecordNotFoundError(result.Error) {
		return tx.Table(table).Create(&desired).Error
	}
	if result.Error != nil {
		return result.Error
	}
	return tx.Table(table).Where("id = ?", desired.ID).Updates(map[string]interface{}{
		"desired_revision": desired.DesiredRevision, "updated_at": desired.UpdatedAt,
	}).Error
}

func (s *ReplicaStore) MarkCache(target TargetPlan, revision string) error {
	return s.updateTarget(target, revision, ReplicaStageCache, true, false, false, "")
}

func (s *ReplicaStore) MarkArtifactCache(target TargetPlan, artifact ContentArtifact, revision string, source string) error {
	return s.MarkArtifactCacheWithAttempts(target, artifact, revision, source, nil)
}

func (s *ReplicaStore) MarkArtifactCacheWithAttempts(target TargetPlan, artifact ContentArtifact, revision string, source string, attempts []ReplicaFetchAttempt) error {
	if s == nil || s.db == nil || !validID(target.TargetID) || !validID(target.InstallationID) ||
		validateStoredArtifact(artifact) != nil || strings.TrimSpace(revision) == "" || !validReplicaFetchSource(source) {
		return ErrInvalidInput
	}
	encoded, err := encodeReplicaFetchAttempts(attempts)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	id := artifactReplicaID(target.TargetID, target.InstallationID, artifact.WorkshopID, artifact.TreeSHA256)
	updates := map[string]interface{}{
		"cached": true, "fetch_source": source, "observed_at": &now, "updated_at": now,
		"error_stage": "", "error_message": "",
	}
	if len(attempts) > 0 {
		updates["fetch_attempts_json"] = encoded
	}
	result := s.db.Table(s.artifactTable).Where("id = ?", id).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *ReplicaStore) MarkArtifactFetchAttempts(target TargetPlan, artifact ContentArtifact, revision string, attempts []ReplicaFetchAttempt) error {
	if s == nil || s.db == nil || !validID(target.TargetID) || !validID(target.InstallationID) ||
		validateStoredArtifact(artifact) != nil || strings.TrimSpace(revision) == "" || len(attempts) == 0 {
		return ErrInvalidInput
	}
	encoded, err := encodeReplicaFetchAttempts(attempts)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	id := artifactReplicaID(target.TargetID, target.InstallationID, artifact.WorkshopID, artifact.TreeSHA256)
	result := s.db.Table(s.artifactTable).Where("id = ?", id).Updates(map[string]interface{}{
		"fetch_attempts_json": encoded, "observed_at": &now, "updated_at": now,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrNotFound
	}
	return nil
}

func encodeReplicaFetchAttempts(attempts []ReplicaFetchAttempt) (string, error) {
	if len(attempts) == 0 {
		return "", nil
	}
	if len(attempts) > 5 {
		return "", ErrInvalidInput
	}
	for _, attempt := range attempts {
		if !validReplicaFetchSource(attempt.Source) ||
			(attempt.Feasibility != "available" && attempt.Feasibility != "unavailable" && attempt.Feasibility != "unknown") ||
			(attempt.Status != "succeeded" && attempt.Status != "failed" && attempt.Status != "unavailable" && attempt.Status != "skipped") ||
			attempt.Bytes < 0 || attempt.DurationMillis < 0 || attempt.BytesPerSecond < 0 || len(attempt.ErrorCode) > 64 || len(attempt.ErrorMessage) > 1024 {
			return "", ErrInvalidInput
		}
	}
	encoded, err := json.Marshal(attempts)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func decodeReplicaFetchAttempts(value string) []ReplicaFetchAttempt {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var attempts []ReplicaFetchAttempt
	if err := json.Unmarshal([]byte(value), &attempts); err != nil || len(attempts) > 5 {
		return nil
	}
	return attempts
}

func validReplicaFetchSource(value string) bool {
	switch strings.TrimSpace(value) {
	case "steam", "peer", "controller", "cache", "legacy_upload":
		return true
	default:
		return false
	}
}

func (s *ReplicaStore) CachedCandidates(workshopID, treeSHA, excludeTargetID string) ([]CachedReplicaCandidate, error) {
	workshopID = strings.TrimSpace(workshopID)
	treeSHA = strings.ToLower(strings.TrimSpace(treeSHA))
	excludeTargetID = strings.TrimSpace(excludeTargetID)
	if s == nil || s.db == nil || !validWorkshopID(workshopID) || !validSHA(treeSHA) {
		return nil, ErrInvalidInput
	}
	query := s.db.Table(s.artifactTable).
		Where("workshop_id = ? AND tree_sha256 = ? AND cached = ?", workshopID, treeSHA, true).
		Order("observed_at DESC, updated_at DESC")
	if excludeTargetID != "" {
		query = query.Where("target_id <> ?", excludeTargetID)
	}
	var records []artifactReplicaRecord
	if err := query.Find(&records).Error; err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	result := make([]CachedReplicaCandidate, 0, len(records))
	for _, record := range records {
		key := record.TargetID + "\x00" + record.InstallationID
		if seen[key] {
			continue
		}
		seen[key] = true
		observedAt := record.UpdatedAt.UTC()
		if record.ObservedAt != nil {
			observedAt = record.ObservedAt.UTC()
		}
		result = append(result, CachedReplicaCandidate{
			TargetID: record.TargetID, InstallationID: record.InstallationID, ObservedAt: observedAt,
		})
	}
	return result, nil
}

func (s *ReplicaStore) MarkPublished(target TargetPlan, revision string) error {
	return s.updateTarget(target, revision, ReplicaStagePublish, true, true, true, "")
}

func (s *ReplicaStore) MarkCompleted(target TargetPlan, revision string) error {
	return s.updateTarget(target, revision, ReplicaStageComplete, true, true, true, "")
}

func (s *ReplicaStore) MarkTargetError(target TargetPlan, revision string, stage ReplicaStage, cause error) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	return s.updateTarget(target, revision, stage, false, false, false, message)
}

func (s *ReplicaStore) MarkRolledBack(target TargetPlan, revision string, cause error) error {
	message := "发布已回滚"
	if cause != nil {
		message = cause.Error()
	}
	return s.updateTarget(target, revision, ReplicaStageRollback, false, false, false, message)
}

func (s *ReplicaStore) updateTarget(target TargetPlan, revision string, stage ReplicaStage, cached, published, configured bool, message string) error {
	if s == nil || s.db == nil || !validID(target.TargetID) || !validID(target.InstallationID) || strings.TrimSpace(revision) == "" {
		return ErrInvalidInput
	}
	now := s.now().UTC()
	stageValue := ""
	if message != "" {
		stageValue = string(stage)
	}
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	rollback := func(err error) error { tx.Rollback(); return err }
	for _, artifact := range target.Mods {
		updates := map[string]interface{}{
			"desired_revision": revision, "observed_at": &now, "updated_at": now,
			"error_stage": stageValue, "error_message": message,
		}
		if stage == ReplicaStageCache || stage == ReplicaStagePublish || stage == ReplicaStageComplete {
			updates["cached"] = cached
		}
		if stage == ReplicaStagePublish || stage == ReplicaStageComplete || stage == ReplicaStageRollback {
			updates["published"] = published
		}
		id := artifactReplicaID(target.TargetID, target.InstallationID, artifact.WorkshopID, artifact.TreeSHA256)
		result := tx.Table(s.artifactTable).Where("id = ?", id).Updates(updates)
		if result.Error != nil {
			return rollback(result.Error)
		}
		if result.RowsAffected != 1 {
			return rollback(ErrNotFound)
		}
	}
	for _, world := range target.Worlds {
		configSHA := hashBytes(world.ModOverrides)
		for _, artifact := range world.Mods {
			updates := map[string]interface{}{
				"desired_revision": revision, "observed_at": &now, "updated_at": now,
				"error_stage": stageValue, "error_message": message,
			}
			if stage == ReplicaStagePublish || stage == ReplicaStageComplete || stage == ReplicaStageRollback {
				updates["configured"] = configured
				if !configured {
					updates["loaded"] = false
				}
			}
			if configured {
				updates["observed_revision"] = revision
			}
			id := worldReplicaID(world.RoomID, world.WorldID, target.TargetID, target.InstallationID, artifact.WorkshopID, artifact.TreeSHA256, configSHA)
			result := tx.Table(s.worldTable).Where("id = ?", id).Updates(updates)
			if result.Error != nil {
				return rollback(result.Error)
			}
			if result.RowsAffected != 1 {
				return rollback(ErrNotFound)
			}
		}
	}
	return tx.Commit().Error
}

func (s *ReplicaStore) MarkWorldLoaded(plan Plan, roomID, worldID string, loaded bool, cause error) error {
	if s == nil || s.db == nil || validatePlan(plan) != nil || !validID(roomID) || !validID(worldID) {
		return ErrInvalidInput
	}
	now := s.now().UTC()
	message, stage := "", ""
	if cause != nil {
		message, stage = cause.Error(), string(ReplicaStageActivate)
	}
	result := s.db.Table(s.worldTable).
		Where("room_id = ? AND world_id = ? AND desired_revision = ?", roomID, worldID, plan.PlanHash).
		Updates(map[string]interface{}{
			"loaded": loaded, "observed_at": &now, "updated_at": now,
			"error_stage": stage, "error_message": message,
		})
	if result.Error != nil {
		return result.Error
	}
	return nil
}

func (s *ReplicaStore) ObserveTarget(target TargetPlan, revision string, observation *InstallationReplicaObservation) error {
	if s == nil || s.db == nil || !validID(target.TargetID) || !validID(target.InstallationID) || strings.TrimSpace(revision) == "" {
		return ErrInvalidInput
	}
	observedAt := s.now().UTC()
	if observation != nil && !observation.ObservedAt.IsZero() {
		observedAt = observation.ObservedAt.UTC()
	}
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	rollback := func(err error) error { tx.Rollback(); return err }
	for _, artifact := range target.Mods {
		published := observation != nil && strings.EqualFold(observation.Mods[artifact.WorkshopID], artifact.TreeSHA256)
		updates := map[string]interface{}{
			"published": published, "observed_at": &observedAt, "updated_at": observedAt,
			"error_stage": "", "error_message": "",
		}
		if published {
			updates["cached"] = true
		}
		id := artifactReplicaID(target.TargetID, target.InstallationID, artifact.WorkshopID, artifact.TreeSHA256)
		result := tx.Table(s.artifactTable).Where("id = ? AND (observed_at IS NULL OR observed_at <= ?)", id, observedAt).Updates(updates)
		if result.Error != nil {
			return rollback(result.Error)
		}
	}
	for _, world := range target.Worlds {
		configSHA := hashBytes(world.ModOverrides)
		observedWorld, worldExists := WorldReplicaObservation{}, false
		if observation != nil {
			observedWorld, worldExists = observation.Worlds[world.RoomID+"/"+world.WorldID]
		}
		for _, artifact := range world.Mods {
			configured := worldExists && strings.EqualFold(observedWorld.ConfigSHA256, configSHA) &&
				strings.EqualFold(observedWorld.Mods[artifact.WorkshopID], artifact.TreeSHA256)
			observedRevision := ""
			if configured {
				observedRevision = revision
			}
			id := worldReplicaID(world.RoomID, world.WorldID, target.TargetID, target.InstallationID, artifact.WorkshopID, artifact.TreeSHA256, configSHA)
			updates := map[string]interface{}{
				"configured": configured, "observed_revision": observedRevision,
				"observed_at": &observedAt, "updated_at": observedAt, "error_stage": "", "error_message": "",
			}
			if !configured {
				updates["loaded"] = false
			}
			result := tx.Table(s.worldTable).Where("id = ? AND (observed_at IS NULL OR observed_at <= ?)", id, observedAt).Updates(updates)
			if result.Error != nil {
				return rollback(result.Error)
			}
		}
	}
	return tx.Commit().Error
}

func (s *ReplicaStore) Room(roomID string) (RoomReplicaState, error) {
	roomID = strings.TrimSpace(roomID)
	if s == nil || s.db == nil || !validID(roomID) {
		return RoomReplicaState{}, ErrInvalidInput
	}
	var worlds []worldReplicaRecord
	if err := s.db.Table(s.worldTable).Where("room_id = ?", roomID).
		Order("workshop_id ASC, target_id ASC, installation_id ASC, world_id ASC").Find(&worlds).Error; err != nil {
		return RoomReplicaState{}, err
	}
	result := RoomReplicaState{RoomID: roomID, Items: []ReplicaModState{}}
	if len(worlds) == 0 {
		return result, nil
	}
	result.DesiredRevision = worlds[0].DesiredRevision
	artifactIDs := make([]string, 0, len(worlds))
	seenArtifacts := make(map[string]bool)
	for _, world := range worlds {
		id := artifactReplicaID(world.TargetID, world.InstallationID, world.WorkshopID, world.TreeSHA256)
		if !seenArtifacts[id] {
			seenArtifacts[id] = true
			artifactIDs = append(artifactIDs, id)
		}
		if world.UpdatedAt.After(timeValue(result.UpdatedAt)) {
			value := world.UpdatedAt.UTC()
			result.UpdatedAt = &value
		}
	}
	var artifacts []artifactReplicaRecord
	if len(artifactIDs) > 0 {
		if err := s.db.Table(s.artifactTable).Where("id IN (?)", artifactIDs).Find(&artifacts).Error; err != nil {
			return RoomReplicaState{}, err
		}
	}
	artifactByID := make(map[string]artifactReplicaRecord, len(artifacts))
	for _, artifact := range artifacts {
		artifactByID[artifact.ID] = artifact
		if artifact.UpdatedAt.After(timeValue(result.UpdatedAt)) {
			value := artifact.UpdatedAt.UTC()
			result.UpdatedAt = &value
		}
	}
	modsByID := make(map[string]*ReplicaModState)
	targetsByMod := make(map[string]map[string]*ReplicaTargetState)
	for _, world := range worlds {
		mod := modsByID[world.WorkshopID]
		if mod == nil {
			mod = &ReplicaModState{WorkshopID: world.WorkshopID, Targets: []ReplicaTargetState{}}
			modsByID[world.WorkshopID] = mod
			targetsByMod[world.WorkshopID] = make(map[string]*ReplicaTargetState)
		}
		targetKey := world.TargetID + "\x00" + world.InstallationID + "\x00" + world.TreeSHA256
		target := targetsByMod[world.WorkshopID][targetKey]
		if target == nil {
			artifact := artifactByID[artifactReplicaID(world.TargetID, world.InstallationID, world.WorkshopID, world.TreeSHA256)]
			target = &ReplicaTargetState{
				TargetID: world.TargetID, InstallationID: world.InstallationID, TreeSHA256: world.TreeSHA256,
				ManifestSHA256: artifact.ManifestSHA256, Size: artifact.Size, FileCount: artifact.FileCount,
				Cached: artifact.Cached, Published: artifact.Published, ObservedAt: utcPointer(artifact.ObservedAt),
				FetchSource: artifact.FetchSource, FetchAttempts: decodeReplicaFetchAttempts(artifact.FetchAttemptsJSON),
				ErrorStage: artifact.ErrorStage, ErrorMessage: artifact.ErrorMessage, Worlds: []ReplicaWorldState{},
			}
			targetsByMod[world.WorkshopID][targetKey] = target
		}
		target.Worlds = append(target.Worlds, ReplicaWorldState{
			RoomID: world.RoomID, WorldID: world.WorldID, Configured: world.Configured, Loaded: world.Loaded,
			ConfigSHA256: world.ConfigSHA256, ObservedRevision: world.ObservedRevision,
			ObservedAt: utcPointer(world.ObservedAt), ErrorStage: world.ErrorStage, ErrorMessage: world.ErrorMessage,
		})
	}
	modIDs := make([]string, 0, len(modsByID))
	for modID := range modsByID {
		modIDs = append(modIDs, modID)
	}
	sort.Strings(modIDs)
	allTargets := make(map[string]bool)
	readyTargets, cachedTargets, publishedTargets := make(map[string]bool), make(map[string]bool), make(map[string]bool)
	worldKeys, configuredWorlds, loadedWorlds := make(map[string]bool), make(map[string]bool), make(map[string]bool)
	for _, modID := range modIDs {
		mod := modsByID[modID]
		targetKeys := make([]string, 0, len(targetsByMod[modID]))
		for key := range targetsByMod[modID] {
			targetKeys = append(targetKeys, key)
		}
		sort.Strings(targetKeys)
		for _, key := range targetKeys {
			target := targetsByMod[modID][key]
			target.Ready = target.Cached && target.Published
			for _, world := range target.Worlds {
				target.Ready = target.Ready && world.Configured
				mod.TotalWorlds++
				if world.Configured {
					mod.ConfiguredWorlds++
				}
				if world.Loaded {
					mod.LoadedWorlds++
				}
				worldKey := world.RoomID + "\x00" + world.WorldID
				worldKeys[worldKey] = true
				if world.Configured {
					configuredWorlds[worldKey] = true
				}
				if world.Loaded {
					loadedWorlds[worldKey] = true
				}
			}
			mod.TotalTargets++
			if target.Ready {
				mod.ReadyTargets++
			}
			mod.Targets = append(mod.Targets, *target)
			installationKey := target.TargetID + "\x00" + target.InstallationID
			allTargets[installationKey] = true
			if target.Cached {
				cachedTargets[installationKey] = true
			}
			if target.Published {
				publishedTargets[installationKey] = true
			}
			if target.Ready {
				readyTargets[installationKey] = true
			}
		}
		result.Items = append(result.Items, *mod)
	}
	result.Coverage = ReplicaCoverage{
		ReadyTargets: len(readyTargets), TotalTargets: len(allTargets), CachedTargets: len(cachedTargets),
		PublishedTargets: len(publishedTargets), ConfiguredWorlds: len(configuredWorlds),
		LoadedWorlds: len(loadedWorlds), TotalWorlds: len(worldKeys),
	}
	return result, nil
}

func artifactReplicaID(targetID, installationID, workshopID, treeSHA string) string {
	return hashBytes([]byte(targetID + "\x00" + installationID + "\x00" + workshopID + "\x00" + strings.ToLower(treeSHA)))
}

func worldReplicaID(roomID, worldID, targetID, installationID, workshopID, treeSHA, configSHA string) string {
	return hashBytes([]byte(strings.Join([]string{roomID, worldID, targetID, installationID, workshopID, strings.ToLower(treeSHA), strings.ToLower(configSHA)}, "\x00")))
}

func timeValue(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}
