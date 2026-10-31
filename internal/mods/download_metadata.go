package mods

import (
	"context"
	"errors"
	"time"
)

// DescribeForDownload is reserved for explicit downloads and dependency walks.
// Normal metadata still uses one batch API request. Only items explicitly
// reported unavailable by Steam may use verified public pages. This must not
// replace Describe in inventory reads or update checks: HTML has no version
// evidence, and a successful result here does not mean the files are current.
func (s *Service) DescribeForDownload(ctx context.Context, modIDs []string) (map[string]SteamMod, error) {
	ids := uniqueModIDs(modIDs)
	if len(ids) > 4096 {
		return nil, ErrInvalidRequest
	}
	for _, id := range ids {
		if !validModID(id) {
			return nil, ErrInvalidModID
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	result := make(map[string]SteamMod, len(ids))
	for start := 0; start < len(ids); start += 100 {
		items, err := s.describeDownloadBatch(ctx, ids[start:min(start+100, len(ids))])
		for id, item := range items {
			result[id] = item
		}
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Service) describeDownloadBatch(ctx context.Context, ids []string) (map[string]SteamMod, error) {
	items, err := s.Describe(ctx, ids)
	if ctx.Err() != nil {
		return items, errors.Join(err, ctx.Err())
	}
	var unavailable *workshopItemUnavailableError
	if !errors.As(err, &unavailable) {
		return items, err
	}
	for _, id := range unavailable.missingIDs {
		if !unavailable.communityIDs[id] {
			return items, err
		}
	}
	for _, id := range unavailable.missingIDs {
		// Details revalidates the exact single-item API result and page identity.
		// Existing Workshop coalescing, response limits and cooldown still apply.
		fresh, pageErr := s.metadata.Details(ctx, []string{id})
		if pageErr != nil {
			return items, pageErr
		}
		item, exists := fresh[id]
		if !exists || item.ID != id {
			return items, err
		}
		normalizeSteamModCollections(&item)
		items[id] = item
	}
	return items, nil
}
