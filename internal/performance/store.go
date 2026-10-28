package performance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
)

type settingRecord struct {
	RoomID string `gorm:"primary_key"`
	Value  string `gorm:"type:text"`
}
type record struct {
	ID                                                                           string `gorm:"primary_key;type:char(64)"`
	RoomID                                                                       string `gorm:"type:varchar(255)"`
	WorldID                                                                      string `gorm:"type:varchar(255)"`
	AtUnix                                                                       int64  `gorm:"index"`
	WorldName, TargetID, InstallationID, InstanceID, State, Reason, ScoreVersion string
	ScoreSum, ValidSeconds, LowSeconds, WindowSeconds                            float64
	MinimumScore                                                                 *float64
	CPUSum                                                                       float64
	CPUCount                                                                     int
	CPUPeak                                                                      *float64
	RSSBytes                                                                     uint64
	Samples                                                                      int
}
type Store struct {
	db                       *gorm.DB
	raw, aggregate, settings string
	mu                       sync.Mutex
}

func NewStore(db *gorm.DB, prefix string) *Store {
	return &Store{db: db, raw: prefix + "performance_sample", aggregate: prefix + "performance_aggregate", settings: prefix + "performance_settings"}
}
func (s *Store) Migrate() error {
	// Separate composite index names for the two tables (SQLite indexes are global).
	for _, table := range []string{s.raw, s.aggregate} {
		if err := s.db.Table(table).AutoMigrate(&record{}).Error; err != nil {
			return err
		}
		if err := s.db.Table(table).AddIndex(table+"_room_time", "room_id", "at_unix").Error; err != nil {
			return err
		}
		if err := s.db.Table(table).AddIndex(table+"_scope_time", "room_id", "world_id", "at_unix").Error; err != nil {
			return err
		}
	}
	return s.db.Table(s.settings).AutoMigrate(&settingRecord{}).Error
}
func (s *Store) Settings(room string) (Settings, error) {
	v := Defaults()
	var row settingRecord
	err := s.db.Table(s.settings).Where("room_id = ?", room).First(&row).Error
	if gorm.IsRecordNotFoundError(err) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	if json.Unmarshal([]byte(row.Value), &v) != nil || !v.valid() {
		return v, ErrInvalid
	}
	return v, nil
}
func (s *Store) SaveSettings(room string, v Settings) (Settings, error) {
	if !v.valid() {
		return v, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := s.Settings(room)
	if err != nil {
		return v, err
	}
	if v.Revision != old.Revision {
		return v, ErrConflict
	}
	v.Revision = uuid.NewString()
	data, _ := json.Marshal(v)
	err = s.db.Table(s.settings).Save(&settingRecord{RoomID: room, Value: string(data)}).Error
	return v, err
}
func recordID(room, world, instance string, at int64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", room, world, instance, at)))
	return hex.EncodeToString(h[:])
}
func (s *Store) Append(p Point) error { return s.appendCurrent(p, "") }
func (s *Store) appendCurrent(p Point, revision string) error {
	if p.ScoreVersion == "" {
		p.ScoreVersion = ScoreVersion
	}
	r := record{RoomID: p.RoomID, WorldID: p.WorldID, AtUnix: p.At.Unix(), WorldName: p.WorldName, TargetID: p.TargetID, InstallationID: p.InstallationID, InstanceID: p.InstanceID, State: p.State, Reason: p.Reason, ScoreVersion: p.ScoreVersion, ValidSeconds: p.ValidSeconds, LowSeconds: p.LowSeconds, WindowSeconds: p.WindowSeconds, MinimumScore: p.Score, RSSBytes: p.RSSBytes, Samples: 1}
	r.ID = recordID(r.RoomID, r.WorldID, r.InstanceID, r.AtUnix)
	if p.Score != nil {
		r.ScoreSum = *p.Score * p.ValidSeconds
	}
	if p.CPUPercent != nil {
		r.CPUSum = *p.CPUPercent
		r.CPUCount = 1
		r.CPUPeak = p.CPUPercent
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision != "" {
		c, err := s.Settings(p.RoomID)
		if err != nil {
			return err
		}
		if !c.Enabled || c.Revision != revision {
			return ErrConflict
		}
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		var existing record
		err := tx.Table(s.raw).Where("id = ?", r.ID).First(&existing).Error
		if err == nil {
			return nil
		}
		if !gorm.IsRecordNotFoundError(err) {
			return err
		}
		if err = tx.Table(s.raw).Create(&r).Error; err != nil {
			return err
		}
		at := r.AtUnix / 300 * 300
		id := recordID(r.RoomID, r.WorldID, r.InstanceID+"\x00"+r.State+"\x00"+r.ScoreVersion, at)
		var a record
		err = tx.Table(s.aggregate).Where("id = ?", id).First(&a).Error
		if err != nil && !gorm.IsRecordNotFoundError(err) {
			return err
		}
		if gorm.IsRecordNotFoundError(err) {
			a = r
			a.ID = id
			a.AtUnix = at
			a.ScoreSum = 0
			a.ValidSeconds = 0
			a.LowSeconds = 0
			a.WindowSeconds = 0
			a.Samples = 0
			a.CPUSum = 0
			a.CPUCount = 0
		}
		a.ScoreSum += r.ScoreSum
		a.ValidSeconds += r.ValidSeconds
		a.LowSeconds += r.LowSeconds
		a.WindowSeconds += r.WindowSeconds
		a.Samples++
		a.CPUSum += r.CPUSum
		a.CPUCount += r.CPUCount
		if r.MinimumScore != nil && (a.MinimumScore == nil || *r.MinimumScore < *a.MinimumScore) {
			a.MinimumScore = r.MinimumScore
		}
		if r.CPUPeak != nil && (a.CPUPeak == nil || *r.CPUPeak > *a.CPUPeak) {
			a.CPUPeak = r.CPUPeak
		}
		if r.RSSBytes > a.RSSBytes {
			a.RSSBytes = r.RSSBytes
		}
		return tx.Table(s.aggregate).Save(&a).Error
	})
}
func (s *Store) Prune(room string, c Settings, now time.Time) error {
	for _, item := range []struct {
		table string
		days  int
	}{{s.raw, c.RetentionDays}, {s.aggregate, c.AggregateDays}} {
		var rows []record
		if err := s.db.Table(item.table).Select("id").Where("room_id = ? AND at_unix < ?", room, now.Add(-time.Duration(item.days)*24*time.Hour).Unix()).Order("at_unix ASC").Limit(2048).Find(&rows).Error; err != nil {
			return err
		}
		ids := []string{}
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		if len(ids) > 0 {
			if err := s.db.Table(item.table).Where("id IN (?)", ids).Delete(&record{}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// PruneExpired bounds retention even for rooms that have been removed. Room-specific
// shorter retention remains enforced by Prune and by the report query itself.
func (s *Store) PruneExpired(now time.Time) error {
	for _, item := range []struct {
		table string
		days  int
	}{{s.raw, 30}, {s.aggregate, 180}} {
		var rows []record
		if err := s.db.Table(item.table).Select("id").Where("at_unix < ?", now.Add(-time.Duration(item.days)*24*time.Hour).Unix()).Order("at_unix ASC").Limit(2048).Find(&rows).Error; err != nil {
			return err
		}
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		if len(ids) > 0 {
			if err := s.db.Table(item.table).Where("id IN (?)", ids).Delete(&record{}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) Report(room string, q Query, worldCount int, now time.Time) (Report, error) {
	result := Report{Items: []Point{}, Worlds: []Summary{}, ScoreVersion: ScoreVersion}
	c, err := s.Settings(room)
	if err != nil {
		return result, err
	}
	if q.To.IsZero() {
		q.To = now
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-24 * time.Hour)
	}
	if q.MaxPoints == 0 {
		q.MaxPoints = 600
	}
	if q.MaxPoints < 30 || q.MaxPoints > 1200 || !q.From.Before(q.To) || q.To.After(now.Add(time.Minute)) {
		return result, ErrInvalid
	}
	cutoff := now.Add(-time.Duration(c.AggregateDays) * 24 * time.Hour)
	if q.From.Before(cutoff) {
		q.From = cutoff
	}
	if !q.From.Before(q.To) {
		return result, ErrInvalid
	}
	result.From = q.From
	result.To = q.To
	table := s.raw
	unit := int64(c.IntervalSeconds)
	if q.From.Before(now.Add(-time.Duration(c.RetentionDays)*24*time.Hour)) || q.To.Sub(q.From) > 4*time.Hour {
		table = s.aggregate
		unit = 300
	}
	if q.WorldID != "" {
		worldCount = 1
	}
	worldCount = max(1, worldCount)
	pointsPerWorld := max(10, q.MaxPoints/worldCount)
	resolution := max(unit, int64(math.Ceil(q.To.Sub(q.From).Seconds()/float64(pointsPerWorld)/float64(unit)))*unit)
	result.ResolutionSeconds = resolution
	query := func() *gorm.DB {
		db := s.db.Table(table).Where("room_id = ? AND at_unix >= ? AND at_unix <= ?", room, q.From.Unix(), q.To.Unix())
		if q.WorldID != "" {
			db = db.Where("world_id = ?", q.WorldID)
		}
		return db
	}
	// Summaries cover the complete retained query range, independent of the
	// plotted point limit. Different scoring algorithms are never averaged.
	sums := "SUM(score_sum) AS score_sum, SUM(valid_seconds) AS valid_seconds, SUM(low_seconds) AS low_seconds, SUM(window_seconds) AS window_seconds, MIN(minimum_score) AS minimum_score, SUM(cpu_sum) AS cpu_sum, SUM(cpu_count) AS cpu_count, MAX(cpu_peak) AS cpu_peak, MAX(rss_bytes) AS rss_bytes, SUM(samples) AS samples"
	var totals []record
	if err := query().Select("world_id, MAX(world_name) AS world_name, score_version, " + sums).Group("world_id,score_version").Scan(&totals).Error; err != nil {
		return result, err
	}
	sort.Slice(totals, func(i, j int) bool {
		if totals[i].WorldID == totals[j].WorldID {
			return totals[i].ScoreVersion < totals[j].ScoreVersion
		}
		return totals[i].WorldID < totals[j].WorldID
	})
	for _, r := range totals {
		a := Summary{WorldID: r.WorldID, WorldName: r.WorldName, ScoreVersion: r.ScoreVersion, MinimumScore: r.MinimumScore, ValidSeconds: r.ValidSeconds, LowSeconds: r.LowSeconds, ObservedSeconds: r.WindowSeconds, PeakCPUPercent: r.CPUPeak, PeakRSSBytes: r.RSSBytes, Findings: []Finding{}}
		a.Coverage = math.Min(1, a.ObservedSeconds/q.To.Sub(q.From).Seconds())
		if a.ValidSeconds > 0 {
			a.AverageScore = number(r.ScoreSum / a.ValidSeconds)
		} else {
			a.Findings = append(a.Findings, Finding{Code: "NO_VALID_SIMULATION_WINDOW", Severity: "info"})
		}
		if a.LowSeconds > 0 {
			a.Findings = append(a.Findings, Finding{Code: "SIMULATION_SLOW", Severity: "warning", Value: a.LowSeconds})
		}
		if a.Coverage < 0.8 {
			a.Findings = append(a.Findings, Finding{Code: "PARTIAL_COVERAGE", Severity: "info", Value: a.Coverage})
		}
		if a.PeakCPUPercent != nil && *a.PeakCPUPercent >= 90 {
			a.Findings = append(a.Findings, Finding{Code: "CPU_CORE_BUSY", Severity: "info", Value: *a.PeakCPUPercent})
		}
		result.Worlds = append(result.Worlds, a)
	}
	var rows []record
	selectSQL := fmt.Sprintf("world_id, world_name, target_id, installation_id, instance_id, state, score_version, MIN(reason) AS reason, (at_unix / %d) * %d AS at_unix, %s", resolution, resolution, sums)
	// Retain recent points if excessive instance/state boundaries exceed the
	// limit; summaries above still include older evidence in the whole range.
	err = query().Select(selectSQL).Group(fmt.Sprintf("world_id,world_name,target_id,installation_id,instance_id,state,score_version,at_unix / %d", resolution)).Order("at_unix DESC, world_id DESC, instance_id DESC, state DESC").Limit(q.MaxPoints + 1).Scan(&rows).Error
	if err != nil {
		return result, err
	}
	result.Truncated = len(rows) > q.MaxPoints
	if result.Truncated {
		rows = rows[:q.MaxPoints]
	}
	slices.Reverse(rows)
	for _, r := range rows {
		p := Point{RoomID: room, WorldID: r.WorldID, WorldName: r.WorldName, TargetID: r.TargetID, InstallationID: r.InstallationID, InstanceID: r.InstanceID, At: time.Unix(r.AtUnix, 0).UTC(), State: r.State, Reason: r.Reason, RSSBytes: r.RSSBytes, WindowSeconds: r.WindowSeconds, ValidSeconds: r.ValidSeconds, LowSeconds: r.LowSeconds, Samples: r.Samples, MinimumScore: r.MinimumScore, ScoreVersion: r.ScoreVersion}
		if r.ValidSeconds > 0 {
			p.Score = number(r.ScoreSum / r.ValidSeconds)
		}
		if r.CPUCount > 0 {
			p.CPUPercent = number(r.CPUSum / float64(r.CPUCount))
		}
		result.Items = append(result.Items, p)
	}
	return result, nil
}
