package notifications

import "time"

type cachedCollection struct {
	Until time.Time
	Value map[string]string
}

// Configured at startup. Read only saved policy, never Runtime or game state.
func (s *Service) ConfigureCollectionStatus(read func(string) map[string]string) {
	s.collectionStatus = read
}
func (s *Service) collection(room string) map[string]string {
	if s.collectionStatus == nil {
		return nil
	}
	s.collectionMu.Lock()
	defer s.collectionMu.Unlock()
	now := time.Now()
	if s.collectionCache == nil {
		s.collectionCache = map[string]cachedCollection{}
	}
	for key, v := range s.collectionCache {
		if !now.Before(v.Until) {
			delete(s.collectionCache, key)
		}
	}
	cached, ok := s.collectionCache[room]
	if !ok {
		cached = cachedCollection{Until: now.Add(time.Minute), Value: s.collectionStatus(room)}
		if len(s.collectionCache) < 128 {
			s.collectionCache[room] = cached
		}
	}
	result := map[string]string{}
	for _, key := range []string{"players", "boss", "alerts"} {
		switch value := cached.Value[key]; value {
		case "enabled", "disabled", "unavailable":
			result[key] = value
		}
	}
	return result
}
