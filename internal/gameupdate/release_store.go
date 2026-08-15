package gameupdate

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

type releaseRecord struct {
	ID                   string `gorm:"primary_key;type:varchar(128)"`
	SourceJobID          string `gorm:"type:varchar(128);unique_index"`
	Stage                string `gorm:"type:varchar(32);index;not null"`
	DesiredVersion       string `gorm:"type:varchar(64);index;not null"`
	TopologyRevision     string `gorm:"type:char(64);index;not null"`
	PlanHash             string `gorm:"type:char(64);index;not null"`
	PlanJSON             string `gorm:"type:text;not null"`
	ProtectionBackupJSON string `gorm:"type:text;not null"`
	ErrorCode            string `gorm:"type:varchar(64)"`
	ErrorMessage         string `gorm:"type:text"`
	FinishedAt           *time.Time
	CreatedAt            time.Time `gorm:"index;not null"`
	UpdatedAt            time.Time `gorm:"not null"`
}

type releaseInstallationRecord struct {
	ID             string `gorm:"primary_key;type:varchar(128)"`
	ReleaseID      string `gorm:"type:varchar(128);index;not null"`
	TargetID       string `gorm:"type:varchar(255);index;not null"`
	InstallationID string `gorm:"type:varchar(255);index;not null"`
	Stage          string `gorm:"type:varchar(32);index;not null"`
	BeforeVersion  string `gorm:"type:varchar(64)"`
	AfterVersion   string `gorm:"type:varchar(64)"`
	Log            string `gorm:"type:text"`
	ErrorCode      string `gorm:"type:varchar(64)"`
	ErrorMessage   string `gorm:"type:text"`
	StartedAt      *time.Time
	FinishedAt     *time.Time
	CreatedAt      time.Time `gorm:"not null"`
	UpdatedAt      time.Time `gorm:"not null"`
}

type releaseShardRecord struct {
	ID              string `gorm:"primary_key;type:varchar(128)"`
	ReleaseID       string `gorm:"type:varchar(128);index;not null"`
	RoomID          string `gorm:"type:varchar(255);index;not null"`
	WorldID         string `gorm:"type:varchar(255);index;not null"`
	TargetID        string `gorm:"type:varchar(255);index;not null"`
	InstallationID  string `gorm:"type:varchar(255);index;not null"`
	IsMaster        bool   `gorm:"not null"`
	WasRunning      bool   `gorm:"not null"`
	Stage           string `gorm:"type:varchar(32);index;not null"`
	RuntimeState    string `gorm:"type:varchar(32)"`
	LoadMarker      string `gorm:"type:varchar(255)"`
	ErrorCode       string `gorm:"type:varchar(64)"`
	ErrorMessage    string `gorm:"type:text"`
	StoppedAt       *time.Time
	StartedAt       *time.Time
	LoadConfirmedAt *time.Time
	CreatedAt       time.Time `gorm:"not null"`
	UpdatedAt       time.Time `gorm:"not null"`
}

type ReleaseStore struct {
	db                *gorm.DB
	releaseTable      string
	installationTable string
	shardTable        string
	now               func() time.Time
}

func NewReleaseStore(db *gorm.DB, tablePrefix string) *ReleaseStore {
	prefix := strings.TrimSpace(tablePrefix)
	return &ReleaseStore{
		db: db, releaseTable: prefix + "game_release", installationTable: prefix + "game_release_installation",
		shardTable: prefix + "game_release_shard", now: time.Now,
	}
}

func (s *ReleaseStore) Migrate() error {
	if s == nil || s.db == nil {
		return ErrReleaseInvalid
	}
	for _, migration := range []struct {
		table string
		model interface{}
	}{{s.releaseTable, &releaseRecord{}}, {s.installationTable, &releaseInstallationRecord{}}, {s.shardTable, &releaseShardRecord{}}} {
		if err := s.db.Table(migration.table).AutoMigrate(migration.model).Error; err != nil {
			return fmt.Errorf("migrate distributed game releases: %w", err)
		}
	}
	now := s.now().UTC()
	active := []string{
		string(ReleaseStageProtecting), string(ReleaseStageStopping), string(ReleaseStageStaged), string(ReleaseStageUpdating),
		string(ReleaseStageVerified), string(ReleaseStageRestarting), string(ReleaseStageConfirming),
	}
	if err := s.db.Table(s.releaseTable).Where("stage IN (?)", active).Updates(map[string]interface{}{
		"stage": string(ReleaseStageRecoveryRequired), "error_code": "SERVICE_RESTARTED",
		"error_message": "服务重启中断了版本发布，请检查各节点版本后执行恢复", "updated_at": now,
	}).Error; err != nil {
		return err
	}
	return nil
}

func (s *ReleaseStore) Create(value Release) (Release, error) {
	if err := validateRelease(value); err != nil {
		return Release{}, err
	}
	record, err := releaseRecordFrom(value)
	if err != nil {
		return Release{}, err
	}
	tx := s.db.Begin()
	if tx.Error != nil {
		return Release{}, tx.Error
	}
	rollback := func(err error) (Release, error) { tx.Rollback(); return Release{}, err }
	if err := tx.Table(s.releaseTable).Create(&record).Error; err != nil {
		return rollback(err)
	}
	for _, installation := range value.Installations {
		record := releaseInstallationRecordFrom(value.ID, installation, value.CreatedAt)
		if err := tx.Table(s.installationTable).Create(&record).Error; err != nil {
			return rollback(err)
		}
	}
	for _, shard := range value.Shards {
		record := releaseShardRecordFrom(value.ID, shard, value.CreatedAt)
		if err := tx.Table(s.shardTable).Create(&record).Error; err != nil {
			return rollback(err)
		}
	}
	if err := tx.Commit().Error; err != nil {
		return Release{}, err
	}
	return s.Get(value.ID)
}

func (s *ReleaseStore) Save(value Release) (Release, error) {
	if err := validateRelease(value); err != nil {
		return Release{}, err
	}
	record, err := releaseRecordFrom(value)
	if err != nil {
		return Release{}, err
	}
	tx := s.db.Begin()
	if tx.Error != nil {
		return Release{}, tx.Error
	}
	rollback := func(err error) (Release, error) { tx.Rollback(); return Release{}, err }
	updates := map[string]interface{}{
		"stage": record.Stage, "protection_backup_json": record.ProtectionBackupJSON, "error_code": record.ErrorCode,
		"error_message": record.ErrorMessage, "finished_at": record.FinishedAt, "updated_at": record.UpdatedAt,
	}
	result := tx.Table(s.releaseTable).Where("id = ? AND plan_hash = ?", record.ID, record.PlanHash).Updates(updates)
	if result.Error != nil {
		return rollback(result.Error)
	}
	if result.RowsAffected != 1 {
		return rollback(ErrReleaseNotFound)
	}
	for _, installation := range value.Installations {
		record := releaseInstallationRecordFrom(value.ID, installation, value.CreatedAt)
		result := tx.Table(s.installationTable).Where("id = ? AND release_id = ?", record.ID, value.ID).Updates(map[string]interface{}{
			"stage": record.Stage, "before_version": record.BeforeVersion, "after_version": record.AfterVersion, "log": record.Log,
			"error_code": record.ErrorCode, "error_message": record.ErrorMessage, "started_at": record.StartedAt,
			"finished_at": record.FinishedAt, "updated_at": record.UpdatedAt,
		})
		if result.Error != nil {
			return rollback(result.Error)
		}
		if result.RowsAffected != 1 {
			return rollback(ErrReleaseNotFound)
		}
	}
	for _, shard := range value.Shards {
		record := releaseShardRecordFrom(value.ID, shard, value.CreatedAt)
		result := tx.Table(s.shardTable).Where("id = ? AND release_id = ?", record.ID, value.ID).Updates(map[string]interface{}{
			"stage": record.Stage, "runtime_state": record.RuntimeState, "load_marker": record.LoadMarker,
			"error_code": record.ErrorCode, "error_message": record.ErrorMessage, "stopped_at": record.StoppedAt,
			"started_at": record.StartedAt, "load_confirmed_at": record.LoadConfirmedAt, "updated_at": record.UpdatedAt,
		})
		if result.Error != nil {
			return rollback(result.Error)
		}
		if result.RowsAffected != 1 {
			return rollback(ErrReleaseNotFound)
		}
	}
	if err := tx.Commit().Error; err != nil {
		return Release{}, err
	}
	return s.Get(value.ID)
}

func (s *ReleaseStore) Get(id string) (Release, error) {
	var record releaseRecord
	result := s.db.Table(s.releaseTable).Where("id = ?", strings.TrimSpace(id)).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Release{}, ErrReleaseNotFound
	}
	if result.Error != nil {
		return Release{}, result.Error
	}
	value, err := releaseFromRecord(record)
	if err != nil {
		return Release{}, err
	}
	var installations []releaseInstallationRecord
	if err := s.db.Table(s.installationTable).Where("release_id = ?", id).Order("target_id ASC, installation_id ASC").Find(&installations).Error; err != nil {
		return Release{}, err
	}
	for _, item := range installations {
		value.Installations = append(value.Installations, releaseInstallationFromRecord(item))
	}
	var shardRecords []releaseShardRecord
	if err := s.db.Table(s.shardTable).Where("release_id = ?", id).Order("room_id ASC, world_id ASC").Find(&shardRecords).Error; err != nil {
		return Release{}, err
	}
	for _, item := range shardRecords {
		value.Shards = append(value.Shards, releaseShardFromRecord(item))
	}
	return value, nil
}

func (s *ReleaseStore) FindIdempotent(id, sourceJobID string) (Release, error) {
	if value, err := s.Get(id); err == nil || !errors.Is(err, ErrReleaseNotFound) {
		return value, err
	}
	if strings.TrimSpace(sourceJobID) == "" {
		return Release{}, ErrReleaseNotFound
	}
	var record releaseRecord
	result := s.db.Table(s.releaseTable).Where("source_job_id = ?", sourceJobID).First(&record)
	if gorm.IsRecordNotFoundError(result.Error) {
		return Release{}, ErrReleaseNotFound
	}
	if result.Error != nil {
		return Release{}, result.Error
	}
	return s.Get(record.ID)
}

func (s *ReleaseStore) List(limit, offset int) ([]Release, int, error) {
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, 0, ErrReleaseInvalid
	}
	var total int
	if err := s.db.Table(s.releaseTable).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var records []releaseRecord
	if err := s.db.Table(s.releaseTable).Order("created_at DESC, id DESC").Limit(limit).Offset(offset).Find(&records).Error; err != nil {
		return nil, 0, err
	}
	values := make([]Release, 0, len(records))
	for _, record := range records {
		value, err := s.Get(record.ID)
		if err != nil {
			return nil, 0, err
		}
		values = append(values, value)
	}
	return values, total, nil
}

func validateRelease(value Release) error {
	if !releaseIdentityPattern.MatchString(value.ID) || value.SourceJobID != "" && !releaseIdentityPattern.MatchString(value.SourceJobID) ||
		validateReleasePlan(value.Plan) != nil || !validReleaseStage(value.Stage) || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() ||
		len(value.Installations) != len(value.Plan.Installations) || len(value.Shards) == 0 {
		return ErrReleaseInvalid
	}
	return nil
}

func validReleaseStage(value ReleaseStage) bool {
	switch value {
	case ReleaseStagePreviewed, ReleaseStageProtecting, ReleaseStageStopping, ReleaseStageStaged, ReleaseStageUpdating,
		ReleaseStageVerified, ReleaseStageRestarting, ReleaseStageConfirming, ReleaseStageSucceeded,
		ReleaseStageFailed, ReleaseStageRecoveryRequired:
		return true
	default:
		return false
	}
}

func releaseRecordFrom(value Release) (releaseRecord, error) {
	plan, err := json.Marshal(value.Plan)
	if err != nil {
		return releaseRecord{}, err
	}
	backups, err := json.Marshal(value.ProtectionBackupIDs)
	if err != nil {
		return releaseRecord{}, err
	}
	return releaseRecord{
		ID: value.ID, SourceJobID: value.SourceJobID, Stage: string(value.Stage), DesiredVersion: value.Plan.DesiredVersion,
		TopologyRevision: value.Plan.TopologyRevision, PlanHash: value.Plan.PlanHash, PlanJSON: string(plan),
		ProtectionBackupJSON: string(backups), ErrorCode: value.ErrorCode, ErrorMessage: value.ErrorMessage,
		FinishedAt: value.FinishedAt, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}, nil
}

func releaseFromRecord(record releaseRecord) (Release, error) {
	value := Release{
		ID: record.ID, SourceJobID: record.SourceJobID, Stage: ReleaseStage(record.Stage), ErrorCode: record.ErrorCode,
		ErrorMessage: record.ErrorMessage, FinishedAt: record.FinishedAt, CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC(),
	}
	if err := json.Unmarshal([]byte(record.PlanJSON), &value.Plan); err != nil {
		return Release{}, err
	}
	if err := json.Unmarshal([]byte(record.ProtectionBackupJSON), &value.ProtectionBackupIDs); err != nil {
		return Release{}, err
	}
	return value, nil
}

func releaseInstallationRecordFrom(releaseID string, value ReleaseInstallationResult, createdAt time.Time) releaseInstallationRecord {
	return releaseInstallationRecord{
		ID: childReleaseRecordID(releaseID, value.TargetID+"\x00"+value.InstallationID), ReleaseID: releaseID,
		TargetID: value.TargetID, InstallationID: value.InstallationID, Stage: string(value.Stage), BeforeVersion: value.BeforeVersion,
		AfterVersion: value.AfterVersion, Log: value.Log, ErrorCode: value.ErrorCode, ErrorMessage: value.ErrorMessage,
		StartedAt: value.StartedAt, FinishedAt: value.FinishedAt, CreatedAt: createdAt, UpdatedAt: value.UpdatedAt,
	}
}

func releaseShardRecordFrom(releaseID string, value ReleaseShardResult, createdAt time.Time) releaseShardRecord {
	return releaseShardRecord{
		ID: childReleaseRecordID(releaseID, value.RoomID+"\x00"+value.WorldID), ReleaseID: releaseID,
		RoomID: value.RoomID, WorldID: value.WorldID, TargetID: value.TargetID, InstallationID: value.InstallationID,
		IsMaster: value.IsMaster, WasRunning: value.WasRunning, Stage: string(value.Stage), RuntimeState: value.RuntimeState,
		LoadMarker: value.LoadMarker, ErrorCode: value.ErrorCode, ErrorMessage: value.ErrorMessage,
		StoppedAt: value.StoppedAt, StartedAt: value.StartedAt, LoadConfirmedAt: value.LoadConfirmedAt,
		CreatedAt: createdAt, UpdatedAt: value.UpdatedAt,
	}
}

func releaseInstallationFromRecord(record releaseInstallationRecord) ReleaseInstallationResult {
	return ReleaseInstallationResult{
		TargetID: record.TargetID, InstallationID: record.InstallationID, Stage: ReleaseStage(record.Stage),
		BeforeVersion: record.BeforeVersion, AfterVersion: record.AfterVersion, Log: record.Log,
		ErrorCode: record.ErrorCode, ErrorMessage: record.ErrorMessage, StartedAt: record.StartedAt,
		FinishedAt: record.FinishedAt, UpdatedAt: record.UpdatedAt.UTC(),
	}
}

func releaseShardFromRecord(record releaseShardRecord) ReleaseShardResult {
	return ReleaseShardResult{
		RoomID: record.RoomID, WorldID: record.WorldID, TargetID: record.TargetID, InstallationID: record.InstallationID,
		IsMaster: record.IsMaster, WasRunning: record.WasRunning, Stage: ReleaseStage(record.Stage), RuntimeState: record.RuntimeState,
		LoadMarker: record.LoadMarker, ErrorCode: record.ErrorCode, ErrorMessage: record.ErrorMessage,
		StoppedAt: record.StoppedAt, StartedAt: record.StartedAt, LoadConfirmedAt: record.LoadConfirmedAt, UpdatedAt: record.UpdatedAt.UTC(),
	}
}

func childReleaseRecordID(releaseID, key string) string {
	digest := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%s:%x", releaseID, digest[:12])
}
