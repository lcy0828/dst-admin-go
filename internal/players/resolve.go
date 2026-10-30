package players

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Resolution never runs Lua or scans saves. All candidates belong to the current
// room's persisted player directory. A partial directory cannot prove uniqueness.
type Resolution struct {
	Status               string   `json:"status"`
	Match                string   `json:"match"`
	Coverage             string   `json:"coverage"`
	Total                int      `json:"total"`
	Offset               int      `json:"offset"`
	HasMore              bool     `json:"hasMore"`
	Items                []Player `json:"items"`
	NormalizationVersion int      `json:"normalizationVersion"`
}

func normalizedName(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Co, r) || unicode.IsControl(r) || r == '\u200b' || r == '\ufeff' {
			return -1
		}
		return r
	}, norm.NFKC.String(value))))
}

func (s *Service) Resolve(roomID, query string, offset int) (Resolution, error) {
	result := Resolution{Status: "not_found", Coverage: "current_directory", Items: []Player{}, Offset: offset, NormalizationVersion: 1}
	query = strings.TrimSpace(query)
	if query == "" || !utf8.ValidString(query) || utf8.RuneCountInString(query) > 128 || offset < 0 || offset > 4096 || strings.ContainsAny(query, "\x00\r\n") {
		return result, ErrInvalidFilter
	}
	room, err := s.managedRoom(roomID)
	if err != nil {
		return result, err
	}
	// Stable IDs use the existing unique room/user index, even for large
	// directories. Name normalization remains a bounded in-memory read.
	var exactRecord playerRecord
	lookup := s.store.db.Table(s.store.table).Where("room_id = ? AND user_id = ?", room.ID, query).First(&exactRecord)
	if lookup.Error == nil && exactRecord.UserID == query {
		result.Status = "resolved"
		result.Match = "exact_id"
		result.Total = 1
		if offset == 0 {
			result.Items = []Player{playerFromRecord(exactRecord)}
		}
		return result, nil
	}
	if lookup.Error != nil && !lookup.RecordNotFound() {
		return result, lookup.Error
	}
	var records []playerRecord
	// Bound work and memory, including Unicode/decorated-name matching. An
	// oversized directory explicitly reports unavailable rather than a false match.
	if err = s.store.db.Table(s.store.table).Where("room_id = ?", room.ID).Order("user_id ASC").Limit(4097).Find(&records).Error; err != nil {
		return result, err
	}
	if len(records) > 4096 {
		result.Status = "unavailable"
		result.Coverage = "directory_limit"
		return result, nil
	}
	var exactID, exact, normalized, fuzzy []Player
	needle := normalizedName(query)
	for _, record := range records {
		p := playerFromRecord(record)
		switch {
		case p.ID == query:
			exactID = append(exactID, p)
		case p.Name == query:
			exact = append(exact, p)
		case needle != "" && normalizedName(p.Name) == needle:
			normalized = append(normalized, p)
		case needle != "" && (strings.Contains(normalizedName(p.Name), needle) || strings.Contains(strings.ToLower(p.ID), strings.ToLower(query))):
			fuzzy = append(fuzzy, p)
		}
	}
	candidates := fuzzy
	result.Match = "substring"
	if len(normalized) > 0 {
		candidates = normalized
		result.Match = "normalized_name"
	}
	if len(exact) > 0 {
		candidates = exact
		result.Match = "exact_name"
	}
	if len(exactID) > 0 {
		candidates = exactID
		result.Match = "exact_id"
	}
	result.Total = len(candidates)
	if result.Total == 0 {
		result.Match = ""
		return result, nil
	}
	result.Status = "ambiguous"
	if result.Total == 1 && result.Match != "substring" {
		result.Status = "resolved"
	}
	start := min(offset, len(candidates))
	end := min(start+5, len(candidates))
	result.Items = candidates[start:end]
	result.HasMore = end < len(candidates)
	return result, nil
}
