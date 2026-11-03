package console

import "strings"

func (s *Store) LocationBinding(room, world string) (string, error) {
	var rows []runRecord
	e := s.db.Table(s.runsTable).Select("id, status").Where("room_id = ? AND world_id = ? AND command_id IN (?)", room, world, []string{"rollback", "regenerate"}).Order("created_at DESC").Limit(16).Find(&rows).Error
	if e != nil {
		return "", e
	}
	ids := []string{"commands-v1"}
	for _, r := range rows {
		if r.Status == string(RunSending) {
			return "", ErrConfirmationNeeded
		}
		ids = append(ids, r.ID+":"+r.Status)
	}
	return strings.Join(ids, "|"), nil
}
