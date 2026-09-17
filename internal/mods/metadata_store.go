package mods

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/jinzhu/gorm"
)

// One latest record per Workshop item and language. Runtime files and room
// settings never enter this table. API version evidence has its own timestamp.
type metadataRecord struct {
	ModID              string `gorm:"primary_key;type:varchar(32)"`
	Language           string `gorm:"primary_key;type:varchar(16)"`
	SummaryJSON        string `gorm:"type:text"`
	SummaryCheckedAt   *time.Time
	CommunityJSON      string `gorm:"type:text"`
	CommunityCheckedAt *time.Time
}

type storedMetadata struct {
	Summary            SteamMod
	SummaryCheckedAt   time.Time
	Community          communityMetadataCache
	CommunityCheckedAt time.Time
}

type MetadataStore struct {
	db    *gorm.DB
	table string
	mu    sync.Mutex
}

func NewMetadataStore(db *gorm.DB, prefix string) *MetadataStore {
	return &MetadataStore{db: db, table: prefix + "workshop_metadata"}
}

func (s *MetadataStore) Migrate() error {
	return s.db.Table(s.table).AutoMigrate(&metadataRecord{}).Error
}

func (s *MetadataStore) read(ids []string, language string) (map[string]storedMetadata, error) {
	result := make(map[string]storedMetadata, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	var rows []metadataRecord
	if err := s.db.Table(s.table).Where("mod_id IN (?) AND language = ?", ids, language).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		var value storedMetadata
		if row.SummaryJSON != "" {
			if err := json.Unmarshal([]byte(row.SummaryJSON), &value.Summary); err != nil {
				return nil, fmt.Errorf("read Workshop %s summary: %w", row.ModID, err)
			}
		}
		if row.CommunityJSON != "" {
			if err := json.Unmarshal([]byte(row.CommunityJSON), &value.Community); err != nil {
				return nil, fmt.Errorf("read Workshop %s display metadata: %w", row.ModID, err)
			}
		}
		if row.SummaryCheckedAt != nil {
			value.SummaryCheckedAt = row.SummaryCheckedAt.UTC()
		}
		if row.CommunityCheckedAt != nil {
			value.CommunityCheckedAt = row.CommunityCheckedAt.UTC()
		}
		result[row.ModID] = value
	}
	return result, nil
}

func (s *MetadataStore) save(id, language string, summary *SteamMod, community *communityMetadataCache, now time.Time) error {
	values := map[string]interface{}{}
	if summary != nil {
		data, err := json.Marshal(summary)
		if err != nil {
			return err
		}
		values["summary_json"], values["summary_checked_at"] = string(data), now.UTC()
	}
	if community != nil {
		data, err := json.Marshal(community)
		if err != nil {
			return err
		}
		values["community_json"], values["community_checked_at"] = string(data), now.UTC()
	}
	// Serialize only these short SQLite upserts, never network requests.
	s.mu.Lock()
	defer s.mu.Unlock()
	row := metadataRecord{ModID: id, Language: language}
	return s.db.Table(s.table).Where("mod_id = ? AND language = ?", id, language).Assign(values).FirstOrCreate(&row).Error
}
