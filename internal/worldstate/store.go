package worldstate

import (
	"fmt"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

const defaultRetention = 2016

type snapshotRecord struct {
	ID                    uint   `gorm:"primary_key"`
	RoomID                string `gorm:"type:varchar(255);not null;index:idx_world_state_room_world_observed"`
	WorldID               string `gorm:"type:varchar(255);not null;index:idx_world_state_room_world_observed"`
	WorldName             string `gorm:"type:varchar(128);not null"`
	WorldRole             string `gorm:"type:varchar(32);not null"`
	Season                string `gorm:"type:varchar(64)"`
	Phase                 string `gorm:"type:varchar(64)"`
	Cycles                *int
	ElapsedDaysInSeason   *int
	RemainingDaysInSeason *int
	SeasonProgress        *float64
	DayProgress           *float64
	PhaseProgress         *float64
	Precipitation         string `gorm:"type:varchar(64)"`
	MoonPhase             string `gorm:"type:varchar(64)"`
	Temperature           *float64
	Wetness               *float64
	Moisture              *float64
	MoistureCeil          *float64
	PrecipitationRate     *float64
	NightmarePhase        string `gorm:"type:varchar(64)"`
	NightmareProgress     *float64
	HostPerformance       *int
	ObservedAt            time.Time `gorm:"not null;index:idx_world_state_room_world_observed"`
}

type Store struct {
	db        *gorm.DB
	table     string
	retention int
}

func NewStore(db *gorm.DB, tablePrefix string) *Store {
	return &Store{db: db, table: strings.TrimSpace(tablePrefix) + "world_state_snapshot", retention: defaultRetention}
}

func (s *Store) Migrate() error {
	if err := s.db.Table(s.table).AutoMigrate(&snapshotRecord{}).Error; err != nil {
		return fmt.Errorf("migrate world state snapshots: %w", err)
	}
	return nil
}

func (s *Store) Append(snapshot Snapshot) (Snapshot, error) {
	record := recordFromSnapshot(snapshot)
	tx := s.db.Begin()
	if tx.Error != nil {
		return Snapshot{}, tx.Error
	}
	rollback := func(err error) (Snapshot, error) {
		tx.Rollback()
		return Snapshot{}, err
	}
	if err := tx.Table(s.table).Create(&record).Error; err != nil {
		return rollback(err)
	}
	if s.retention > 0 {
		var snapshotIDs []uint
		if err := tx.Table(s.table).
			Where("room_id = ? AND world_id = ?", snapshot.RoomID, snapshot.WorldID).
			Order("observed_at DESC, id DESC").Pluck("id", &snapshotIDs).Error; err != nil {
			return rollback(err)
		}
		var staleIDs []uint
		if len(snapshotIDs) > s.retention {
			staleIDs = snapshotIDs[s.retention:]
		}
		if len(staleIDs) > 0 {
			if err := tx.Table(s.table).Where("id IN (?)", staleIDs).Delete(&snapshotRecord{}).Error; err != nil {
				return rollback(err)
			}
		}
	}
	if err := tx.Commit().Error; err != nil {
		return Snapshot{}, err
	}
	return snapshotFromRecord(record), nil
}

func (s *Store) Current(roomID string) ([]Snapshot, error) {
	var ids []uint
	if err := s.db.Table(s.table).Where("room_id = ?", roomID).Select("MAX(id)").Group("world_id").Pluck("MAX(id)", &ids).Error; err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []Snapshot{}, nil
	}
	var records []snapshotRecord
	if err := s.db.Table(s.table).Where("id IN (?)", ids).Order("world_name ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	items := make([]Snapshot, 0, len(records))
	for _, record := range records {
		items = append(items, snapshotFromRecord(record))
	}
	return items, nil
}

func (s *Store) History(roomID, worldID string, limit int) ([]Snapshot, int, error) {
	query := s.db.Table(s.table).Where("room_id = ? AND world_id = ?", roomID, worldID)
	var total int
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var records []snapshotRecord
	if err := query.Order("observed_at DESC, id DESC").Limit(limit).Find(&records).Error; err != nil {
		return nil, 0, err
	}
	items := make([]Snapshot, 0, len(records))
	for _, record := range records {
		items = append(items, snapshotFromRecord(record))
	}
	return items, total, nil
}

func recordFromSnapshot(snapshot Snapshot) snapshotRecord {
	return snapshotRecord{
		RoomID: snapshot.RoomID, WorldID: snapshot.WorldID, WorldName: snapshot.WorldName, WorldRole: snapshot.WorldRole,
		Season: snapshot.Season, Phase: snapshot.Phase, Cycles: snapshot.Cycles,
		ElapsedDaysInSeason: snapshot.ElapsedDaysInSeason, RemainingDaysInSeason: snapshot.RemainingDaysInSeason,
		SeasonProgress: snapshot.SeasonProgress, DayProgress: snapshot.DayProgress, PhaseProgress: snapshot.PhaseProgress,
		Precipitation: snapshot.Precipitation, MoonPhase: snapshot.MoonPhase, Temperature: snapshot.Temperature,
		Wetness: snapshot.Wetness, Moisture: snapshot.Moisture, MoistureCeil: snapshot.MoistureCeil,
		PrecipitationRate: snapshot.PrecipitationRate, NightmarePhase: snapshot.NightmarePhase,
		NightmareProgress: snapshot.NightmareProgress, HostPerformance: snapshot.HostPerformance,
		ObservedAt: snapshot.ObservedAt.UTC(),
	}
}

func snapshotFromRecord(record snapshotRecord) Snapshot {
	return Snapshot{
		ID: record.ID, RoomID: record.RoomID, WorldID: record.WorldID, WorldName: record.WorldName, WorldRole: record.WorldRole,
		Season: record.Season, Phase: record.Phase, Cycles: record.Cycles,
		ElapsedDaysInSeason: record.ElapsedDaysInSeason, RemainingDaysInSeason: record.RemainingDaysInSeason,
		SeasonProgress: record.SeasonProgress, DayProgress: record.DayProgress, PhaseProgress: record.PhaseProgress,
		Precipitation: record.Precipitation, MoonPhase: record.MoonPhase, Temperature: record.Temperature,
		Wetness: record.Wetness, Moisture: record.Moisture, MoistureCeil: record.MoistureCeil,
		PrecipitationRate: record.PrecipitationRate, NightmarePhase: record.NightmarePhase,
		NightmareProgress: record.NightmareProgress, HostPerformance: record.HostPerformance,
		ObservedAt: record.ObservedAt.UTC(),
	}
}
