package mods

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"
)

const displayLanguage = "schinese"
const displayVersionTTL = 5 * time.Minute

func (p *SteamProvider) ConfigureMetadataStore(store *MetadataStore) { p.store = store }

// CachedMetadata is display-only: no network, version check, or TTL refresh.
func (p *SteamProvider) CachedMetadata(ctx context.Context, ids []string) (map[string]SteamMod, error) {
	result := make(map[string]SteamMod)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.store == nil {
		return result, nil
	}
	records, err := p.store.read(ids, displayLanguage)
	if err != nil {
		return nil, err
	}
	for id, record := range records {
		item := record.Summary
		mergeCommunityMetadata(&item, record.Community)
		// Cached version evidence must not be used as an update check.
		result[id] = SteamMod{ID: id, Name: item.Name, Author: item.Author, PreviewURL: item.PreviewURL}
	}
	return result, nil
}

// Lists keep known names/authors regardless of age. Opening details refreshes
// old display fields; version checks always use Summaries and bypass this TTL.
func (p *SteamProvider) DisplayMetadata(ctx context.Context, ids []string) (map[string]SteamMod, error) {
	if p.store == nil {
		return p.Details(ctx, ids)
	}
	return p.storedDetails(ctx, ids, false)
}

func (p *SteamProvider) storedDetails(ctx context.Context, ids []string, refreshOld bool) (map[string]SteamMod, error) {
	ids = uniqueModIDs(ids)
	if len(ids) > 100 {
		return nil, ErrInvalidRequest
	}
	// The same room is often opened in several panels at once. Recheck the DB
	// after acquiring this gate so those panels share the batch API response.
	select {
	case p.summarySlot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	records, err := p.store.read(ids, displayLanguage)
	if err != nil {
		<-p.summarySlot
		return nil, err
	}
	result := make(map[string]SteamMod, len(ids))
	missing := make([]string, 0)
	for _, id := range ids {
		record := records[id]
		result[id] = record.Summary
		if record.SummaryCheckedAt.IsZero() || time.Since(record.SummaryCheckedAt) >= displayVersionTTL {
			missing = append(missing, id)
		}
	}
	var failures []error
	if len(missing) > 0 {
		fresh, fetchErr := p.Summaries(ctx, missing)
		failures = append(failures, fetchErr)
		for _, id := range missing {
			if item, ok := fresh[id]; ok {
				result[id] = item
			} else {
				// Keep known display fields, but never label stale API evidence current.
				item := result[id]
				item.Version, item.SteamManifestID, item.UpdatedAt = "", "", time.Time{}
				result[id] = item
			}
		}
	}
	<-p.summarySlot
	items := make([]SteamMod, 0, len(ids))
	for _, id := range ids {
		item := result[id]
		record := records[id]
		if item.ID == "" && record.Community.Name == "" && record.Community.Author == "" {
			delete(result, id)
			continue
		}
		item.ID = id
		if record.Community.Name != "" || record.Community.Author != "" {
			mergeCommunityMetadata(&item, record.Community)
			p.cacheMu.Lock()
			if _, exists := p.communityCache[id]; !exists {
				p.communityCache[id] = record.Community
			}
			p.cacheMu.Unlock()
		}
		complete := record.Community.Name != "" && record.Community.Author != ""
		if !complete || refreshOld && time.Since(record.CommunityCheckedAt) >= steamCommunityCacheTTL {
			items = append(items, item)
		}
		result[id] = item
	}
	if len(items) > 0 {
		failures = append(failures, p.populateCommunityMetadata(ctx, items))
		for _, item := range items {
			result[item.ID] = item
		}
	}
	return result, errors.Join(failures...)
}

func (p *SteamProvider) rememberSummaries(items []SteamMod) {
	if p.store == nil {
		return
	}
	for _, item := range items {
		if err := p.store.save(item.ID, displayLanguage, &item, nil, time.Now()); err != nil {
			log.Printf("[WorkshopMetadata] save summary id=%s: %v", item.ID, err)
		}
	}
}

func (p *SteamProvider) rememberSearch(items []SteamMod, language string) {
	if p.store == nil || len(items) == 0 {
		return
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	records, err := p.store.read(ids, language)
	if err != nil {
		log.Printf("[WorkshopMetadata] read search metadata: %v", err)
		return
	}
	for _, item := range items {
		// A search card is partial evidence. It must not replace a full description
		// or postpone the next details refresh, nor count as a version check.
		record := records[item.ID]
		if record.Community.Name != "" && record.Community.Author != "" {
			continue
		}
		value := record.Community
		if strings.TrimSpace(item.Name) != "" {
			value.Name = item.Name
		}
		if strings.TrimSpace(item.Author) != "" {
			value.Author = item.Author
		}
		if err := p.store.save(item.ID, language, nil, &value, record.CommunityCheckedAt); err != nil {
			log.Printf("[WorkshopMetadata] save search id=%s: %v", item.ID, err)
		}
	}
}
