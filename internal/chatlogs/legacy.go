package chatlogs

import (
	"fmt"

	"dont/internal/rooms"
	"dont/internal/runtimefiles"
	"dont/shared"

	"github.com/jinzhu/gorm"
)

// Old IDs identified a boot, not a chat file. Never inherit their byte cursor:
// replay the new file and adopt only positively matched persisted sources.
// The binding and each source move commit with the new batch checkpoint.
func (s *Store) bindLegacyGeneration(tx *gorm.DB, roomID, worldID string, generation shared.RuntimeChatLogGeneration, existing generationRecord, entries []parsedEntry) (string, error) {
	if existing.LegacyGenerationID != "" {
		return existing.LegacyGenerationID, nil
	}
	legacyID := runtimefiles.LegacyChatGenerationID(generation.ID)
	if legacyID == "" {
		return "", nil
	}
	var legacy generationRecord
	err := tx.Table(s.generationsTable).Where("room_id = ? AND world_id = ? AND generation_id = ?", roomID, worldID, legacyID).First(&legacy).Error
	var candidates []generationRecord
	if err == nil {
		candidates = append(candidates, legacy)
	} else if !gorm.IsRecordNotFoundError(err) {
		return "", err
	}
	if generation.Archived {
		// Copying a completed chat into the archive can change its inode. Only
		// an absent source file may be adopted; a new live file is independent.
		var copies []generationRecord
		if err := tx.Table(s.generationsTable).Where(
			"room_id = ? AND world_id = ? AND unavailable = ? AND generation_id LIKE ? AND (file_name = ? OR file_name = ?)",
			roomID, worldID, true, "chat-v2:"+legacyID+":%", generation.FileName, "server_chat_log.txt",
		).Limit(17).Find(&copies).Error; err != nil {
			return "", err
		}
		if len(copies) > 16 {
			return "", fmt.Errorf("聊天归档存在过多待核验来源，尚未确认同步完成")
		}
		candidates = append(candidates, copies...)
	}
	binding := ""
	for _, candidate := range candidates {
		matches, err := s.canBindLegacyGeneration(tx, roomID, worldID, generation, candidate, entries)
		if err != nil {
			return "", err
		}
		if matches {
			if binding != "" {
				return "", fmt.Errorf("聊天归档存在多个匹配来源，尚未确认同步完成")
			}
			binding = candidate.GenerationID
		}
	}
	return binding, nil
}

func (s *Store) canBindLegacyGeneration(tx *gorm.DB, roomID, worldID string, generation shared.RuntimeChatLogGeneration, legacy generationRecord, entries []parsedEntry) (bool, error) {
	if legacy.Archived && legacy.FileName != generation.FileName || legacy.Cursor > generation.Size {
		return false, nil
	}
	var bound int
	if err := tx.Table(s.generationsTable).Where("room_id = ? AND world_id = ? AND legacy_generation_id = ?", roomID, worldID, legacy.GenerationID).Count(&bound).Error; err != nil || bound > 0 {
		return false, err
	}
	var first sourceRecord
	err := tx.Table(s.sourcesTable).Where("room_id = ? AND world_id = ? AND generation_id = ?", roomID, worldID, legacy.GenerationID).Order("source_cursor ASC").First(&first).Error
	if gorm.IsRecordNotFoundError(err) {
		return legacy.Cursor == 0 && legacy.Size == 0 && legacy.FileName == generation.FileName, nil
	}
	if err != nil {
		return false, err
	}
	if len(entries) == 0 || first.SourceCursor != entries[0].sourceCursor {
		return false, nil
	}
	return s.legacySourceMatches(tx, first, entries[0])
}

func (s *Store) legacySourceMatches(tx *gorm.DB, source sourceRecord, entry parsedEntry) (bool, error) {
	if source.SourceTimestamp != entry.SourceTimestamp || source.SourceCursor != entry.sourceCursor {
		return false, nil
	}
	var event eventRecord
	if err := tx.Table(s.eventsTable).Where("id = ?", source.EventID).First(&event).Error; err != nil {
		return false, err
	}
	return event.Fingerprint == entryIdentity(entry), nil
}

func (s *Store) adoptLegacySource(tx *gorm.DB, roomID string, world rooms.World, generationID, legacyID string, entry parsedEntry) (*sourceRecord, error) {
	if legacyID == "" {
		return nil, nil
	}
	var source sourceRecord
	err := tx.Table(s.sourcesTable).Where("id = ?", chatSourceID(roomID, world.ID, legacyID, entry.sourceCursor)).First(&source).Error
	if gorm.IsRecordNotFoundError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	matches, err := s.legacySourceMatches(tx, source, entry)
	if err != nil {
		return nil, err
	}
	if !matches {
		return nil, fmt.Errorf("聊天历史分段核验失败，保留已有记录并等待核验")
	}
	id := chatSourceID(roomID, world.ID, generationID, entry.sourceCursor)
	if err := tx.Table(s.sourcesTable).Where("id = ?", source.ID).Updates(map[string]interface{}{"id": id, "generation_id": generationID}).Error; err != nil {
		return nil, err
	}
	source.ID, source.GenerationID = id, generationID
	// Reconcile event time only after the new generation row exists: source
	// selection joins that row and would otherwise treat the event as orphaned.
	return &source, nil
}

func (s *Store) removeEmptyLegacyGeneration(tx *gorm.DB, roomID, worldID, legacyID string) error {
	var remaining int
	if err := tx.Table(s.sourcesTable).Where("room_id = ? AND world_id = ? AND generation_id = ?", roomID, worldID, legacyID).Count(&remaining).Error; err != nil || remaining > 0 {
		return err
	}
	return tx.Table(s.generationsTable).Where("room_id = ? AND world_id = ? AND generation_id = ?", roomID, worldID, legacyID).Delete(&generationRecord{}).Error
}
