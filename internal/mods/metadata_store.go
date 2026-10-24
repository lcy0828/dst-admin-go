package mods

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/jinzhu/gorm"
)

// One latest record per Workshop item and language. Runtime files and room
// settings never enter this table. API version evidence has its own timestamp.
type metadataRecord struct {
	Scope              string `gorm:"primary_key;type:varchar(64)"`
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
	db           *gorm.DB
	table        string
	mu           *sync.Mutex
	scope        string
	legacy       string
	writes       *int
	legacyExists bool
}

func NewMetadataStore(db *gorm.DB, prefix string) *MetadataStore {
	return &MetadataStore{db: db, table: prefix + "workshop_metadata_v2", mu: &sync.Mutex{}, scope: "legacy", legacy: prefix + "workshop_metadata", writes: new(int)}
}

func (s *MetadataStore) Migrate() error {
	if err := s.db.Table(s.table).AutoMigrate(&metadataRecord{}).Error; err != nil {
		return err
	}
	s.legacyExists = s.db.HasTable(s.legacy)
	return nil
}

func (s *MetadataStore) read(ids []string, language string) (map[string]storedMetadata, error) {
	result := make(map[string]storedMetadata, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	var rows []metadataRecord
	if err := s.db.Table(s.table).Where("scope = ? AND mod_id IN (?) AND language = ?", s.scope, ids, language).Find(&rows).Error; err != nil {
		return nil, err
	}
	if s.scope == "legacy" && s.legacyExists && len(rows) < len(ids) {
		var old []metadataRecord
		if err := s.db.Table(s.legacy).Where("mod_id IN (?) AND language = ?", ids, language).Find(&old).Error; err == nil {
			found := make(map[string]bool)
			for _, row := range rows {
				found[row.ModID] = true
			}
			for _, row := range old {
				if !found[row.ModID] {
					row.Scope = "historical"
					row.SummaryCheckedAt = nil
					rows = append(rows, row)
				}
			}
		}
	}
	for _, row := range rows {
		if len(row.SummaryJSON) > 128<<10 || len(row.CommunityJSON) > 128<<10 {
			continue
		}
		var value storedMetadata
		if row.SummaryJSON != "" {
			if err := json.Unmarshal([]byte(row.SummaryJSON), &value.Summary); err != nil {
				continue
			}
		}
		if row.CommunityJSON != "" {
			if err := json.Unmarshal([]byte(row.CommunityJSON), &value.Community); err != nil {
				continue
			}
		}
		if row.SummaryCheckedAt != nil {
			value.SummaryCheckedAt = row.SummaryCheckedAt.UTC()
		}
		if row.CommunityCheckedAt != nil {
			value.CommunityCheckedAt = row.CommunityCheckedAt.UTC()
		}
		if row.Scope == "historical" {
			item := value.Summary
			value.Summary = SteamMod{ID: row.ModID, Name: item.Name, Author: item.Author, PreviewURL: item.PreviewURL}
		}
		result[row.ModID] = value
	}
	if s.scope != "legacy" && len(result) < len(ids) {
		legacy := s.WithScope("legacy")
		historical, err := legacy.read(ids, language)
		if err == nil {
			for id, item := range historical {
				if _, exists := result[id]; !exists {
					summary := item.Summary
					item.Summary = SteamMod{ID: id, Name: summary.Name, Author: summary.Author, PreviewURL: summary.PreviewURL}
					item.SummaryCheckedAt = time.Time{}
					result[id] = item
				}
			}
		}
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
		if len(data) <= 128<<10 {
			values["summary_json"], values["summary_checked_at"] = string(data), now.UTC()
		}
	}
	if community != nil {
		data, err := json.Marshal(community)
		if err != nil {
			return err
		}
		if len(data) <= 128<<10 {
			values["community_json"], values["community_checked_at"] = string(data), now.UTC()
		}
	}
	// Serialize only these short SQLite upserts, never network requests.
	s.mu.Lock()
	defer s.mu.Unlock()
	row := metadataRecord{Scope: s.scope, ModID: id, Language: language}
	if len(values) == 0 {
		return nil
	}
	if err := s.db.Table(s.table).Where("scope = ? AND mod_id = ? AND language = ?", s.scope, id, language).Assign(values).FirstOrCreate(&row).Error; err != nil {
		return err
	}
	*s.writes++
	if *s.writes%32 == 0 {
		var usage struct {
			Count int
			Bytes int64
		}
		sizeSQL := "COALESCE(LENGTH(summary_json),0)+COALESCE(LENGTH(community_json),0)"
		if err := s.db.Table(s.table).Select("COUNT(*) AS count, COALESCE(SUM(" + sizeSQL + "),0) AS bytes").Scan(&usage).Error; err == nil && (usage.Count > 8192 || usage.Bytes > 32<<20) {
			// SQLite cache only; one ordered index scan and one bounded bulk delete.
			var candidates []struct {
				RowID int64
				Bytes int64
			}
			if err := s.db.Table(s.table).Select("rowid AS row_id, " + sizeSQL + " AS bytes").Order("COALESCE(summary_checked_at, community_checked_at) ASC").Scan(&candidates).Error; err == nil {
				remove := []int64{}
				for _, entry := range candidates {
					if usage.Count <= 8192 && usage.Bytes <= 32<<20 {
						break
					}
					remove = append(remove, entry.RowID)
					usage.Count--
					usage.Bytes -= entry.Bytes
				}
				if len(remove) > 0 {
					s.db.Table(s.table).Where("rowid IN (?)", remove).Delete(&metadataRecord{})
				}
			}
		}
	}

	return nil
}

func (s *MetadataStore) WithScope(scope string) *MetadataStore {
	if scope == "" {
		scope = "legacy"
	}
	return &MetadataStore{db: s.db, table: s.table, mu: s.mu, scope: scope, legacy: s.legacy, writes: s.writes, legacyExists: s.legacyExists}
}
