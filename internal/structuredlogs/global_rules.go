package structuredlogs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jinzhu/gorm"
)

type ruleMigrationRecord struct {
	ID        string `gorm:"primary_key"`
	CreatedAt time.Time
}

const globalRulesMigration = "global-rules-v1"

// migrateGlobalRules copies room rules into the global scope once. The original
// rows remain untouched; the marker prevents deleted global rules from being
// restored from those archived rows on subsequent starts.
func (s *Store) migrateGlobalRules() error {
	if err := s.db.Table(s.ruleMigrationsTable).AutoMigrate(&ruleMigrationRecord{}).Error; err != nil {
		return fmt.Errorf("migrate log rule migration state: %w", err)
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		var marker ruleMigrationRecord
		err := tx.Table(s.ruleMigrationsTable).Where("id = ?", globalRulesMigration).First(&marker).Error
		if err == nil {
			return nil
		}
		if !gorm.IsRecordNotFoundError(err) {
			return err
		}
		var records []ruleRecord
		// Existing global choices take precedence. For colliding room rule IDs,
		// keep the most recently edited copy and preserve other variants as custom rules.
		if err := tx.Table(s.rulesTable).Order("CASE WHEN room_id = '' THEN 0 ELSE 1 END, updated_at DESC, room_id ASC, id ASC").Find(&records).Error; err != nil {
			return err
		}
		ids := make(map[string]struct{}, len(records))
		fingerprints := make(map[string]struct{}, len(records))
		for _, record := range records {
			if record.RoomID == "" {
				ids[record.ID] = struct{}{}
			}
		}
		for _, record := range records {
			fingerprint := globalRuleFingerprint(ruleFromRecord(record))
			if record.RoomID == "" {
				fingerprints[fingerprint] = struct{}{}
				continue
			}
			if _, exists := fingerprints[fingerprint]; exists {
				continue
			}
			fingerprints[fingerprint] = struct{}{}
			if _, exists := ids[record.ID]; exists {
				// Include the source key so the ID cannot collide even with a
				// user rule deliberately named after a previous migration ID.
				digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%s", record.Key, record.RoomID, fingerprint)))
				base := "merged-" + hex.EncodeToString(digest[:16])
				record.ID = base
				for suffix := 1; ; suffix++ {
					if _, used := ids[record.ID]; !used {
						break
					}
					record.ID = fmt.Sprintf("%s-%d", base, suffix)
				}
				// An enabled older variant could silently undo a newer disabled
				// rule. Retain it for review without changing live classification;
				// the original row still preserves its exact enabled state.
				record.BuiltIn, record.Enabled = false, false
				record.Name = retainedRuleName(record.Name)
			}
			ids[record.ID] = struct{}{}
			record.Key, record.RoomID = 0, ""
			if err := tx.Table(s.rulesTable).Create(&record).Error; err != nil {
				return err
			}
		}
		return tx.Table(s.ruleMigrationsTable).Create(&ruleMigrationRecord{ID: globalRulesMigration, CreatedAt: s.now().UTC()}).Error
	})
}

func retainedRuleName(name string) string {
	const suffix = "（合并保留）"
	runes := []rune(name)
	if len(runes)+len([]rune(suffix)) > 80 {
		runes = runes[:80-len([]rune(suffix))]
	}
	return string(runes) + suffix
}

// Compare all user-editable fields, including disabled status and priority.
// Built-in identities stay distinct so initialization never restores a second
// enabled copy of a built-in that the user disabled.
func globalRuleFingerprint(rule Rule) string {
	builtInID := ""
	if rule.BuiltIn {
		builtInID = rule.ID
	}
	value, _ := json.Marshal(struct {
		RuleInput
		BuiltInID string
	}{
		RuleInput: RuleInput{Name: rule.Name, Description: rule.Description, LogType: rule.LogType,
			Pattern: rule.Pattern, Regex: rule.Regex, Enabled: rule.Enabled, Priority: rule.Priority,
			MatchMode: rule.MatchMode, TailPattern: rule.TailPattern},
		BuiltInID: builtInID,
	})
	return string(value)
}
