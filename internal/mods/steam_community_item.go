package mods

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const workshopCommunityWarning = "工坊 API 未提供完整资料，已使用公开工坊页面；版本与更新清单未确认，依赖以页面声明为准。"

// Keep the API's business result separate from transport and authorization
// errors. Only exact, unambiguous EResult 9 records authorize page inspection.
// Batch/version reads keep the error; an explicit download may inspect each
// eligible item separately under the existing page request limits.
type workshopItemUnavailableError struct {
	missingIDs   []string
	communityIDs map[string]bool
}

func (e *workshopItemUnavailableError) Error() string { return e.Unwrap().Error() }
func (e *workshopItemUnavailableError) Unwrap() error {
	return &NetworkError{Code: "WORKSHOP_ITEM_UNAVAILABLE", Status: 404}
}

var communitySubscribePattern = regexp.MustCompile(`^\s*SubscribeItem\(\s*('[0-9]+'|"[0-9]+")\s*,\s*('[0-9]+'|"[0-9]+")\s*\)\s*;?\s*$`)

// Read only the primary subscription button and the declared required-items
// section. Steam pages may also embed JSON for unrelated images/collections;
// those IDs and app IDs are not evidence about the requested Mod.
func readCommunityItem(document *goquery.Document, data []byte, metadata *communityMetadataCache) {
	metadata.ItemCheckedAt = time.Now().UTC()
	button := document.Find("#SubscribeItemBtn")
	if button.Length() != 1 || button.ParentsFiltered("#highlightContent, .workshopItemDescription").Length() != 0 ||
		document.Find("#highlightContent").Length() != 1 {
		return
	}
	if _, disabled := button.Attr("disabled"); disabled || button.HasClass("disabled") || button.AttrOr("aria-disabled", "") == "true" {
		return
	}
	match := communitySubscribePattern.FindStringSubmatch(button.AttrOr("onclick", ""))
	if len(match) != 3 {
		return
	}
	itemID, appID := strings.Trim(match[1], `"'`), strings.Trim(match[2], `"'`)
	if !validModID(itemID) {
		return
	}
	// An absent required-items section is meaningful only on a complete page;
	// a truncated document must not silently mean "no dependencies".
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) < 7 || !bytes.EqualFold(trimmed[len(trimmed)-7:], []byte("</html>")) {
		return
	}
	required := document.Find("#RequiredItems")
	if required.Length() > 1 || required.Find("a").Length() > 100 || required.ParentsFiltered("#highlightContent, .workshopItemDescription").Length() != 0 {
		return
	}
	dependencies := []string{}
	valid := true
	required.Find("a").EachWithBreak(func(_ int, link *goquery.Selection) bool {
		u, err := url.Parse(link.AttrOr("href", ""))
		if err != nil || u.User != nil || u.Fragment != "" ||
			!(u.Scheme == "" && u.Host == "" || u.Scheme == "https" && strings.EqualFold(u.Host, "steamcommunity.com")) ||
			(u.Path != "/sharedfiles/filedetails/" && u.Path != "/workshop/filedetails/") {
			valid = false
			return false
		}
		query, err := url.ParseQuery(u.RawQuery)
		if err != nil || len(query["id"]) != 1 || !validModID(query.Get("id")) {
			valid = false
			return false
		}
		dependencies = append(dependencies, query.Get("id"))
		return true
	})
	if !valid {
		return
	}
	// Never publish an incomplete dependency list when an item lacks its link.
	if required.Find(".requiredItem").Length() > len(dependencies) {
		return
	}
	metadata.ItemID, metadata.AppID = itemID, appID
	metadata.Dependencies = append([]string{}, uniqueModIDs(dependencies)...)
	metadata.PreviewURL = document.Find(`meta[property="og:image"]`).First().AttrOr("content", "")
}

func (p *SteamProvider) withCommunityFallback(ctx context.Context, ids []string, items map[string]SteamMod, apiErr error) (map[string]SteamMod, error) {
	var unavailable *workshopItemUnavailableError
	if len(ids) != 1 || !errors.As(apiErr, &unavailable) || !unavailable.communityIDs[ids[0]] || ctx.Err() != nil {
		return items, apiErr
	}
	// The long display TTL must not preserve old subscription eligibility or
	// dependencies for a newly unavailable item. Reuse a recent verification,
	// but refresh old/legacy display-only entries under the existing gate.
	p.cacheMu.Lock()
	if cached, exists := p.communityCache[ids[0]]; exists && cached.Err == nil &&
		(cached.ItemCheckedAt.IsZero() || time.Since(cached.ItemCheckedAt) >= displayVersionTTL) {
		cached.ExpiresAt = time.Now()
		p.cacheCommunityLocked(ids[0], cached)
	}
	p.cacheMu.Unlock()
	metadata, ok := p.communityMetadata(ctx, ids[0])
	if metadata.Err != nil {
		return items, errors.Join(metadata.Err, apiErr)
	}
	if !ok || metadata.ItemID != ids[0] || metadata.AppID != p.AppID || metadata.Dependencies == nil ||
		strings.TrimSpace(metadata.Name) == "" || len(metadata.Name) > 4096 || len(metadata.Author) > 512 ||
		len(metadata.Description) > 64<<10 || len(metadata.PreviewURL) > 8192 {
		return items, apiErr
	}
	// Display and declared dependencies only. No version, manifest, timestamps,
	// subscription counts or downloaded/installed status are inferred from HTML.
	item := SteamMod{ID: ids[0], PreviewURL: metadata.PreviewURL,
		MetadataWarning: workshopCommunityWarning, Dependencies: append([]string{}, metadata.Dependencies...)}
	mergeCommunityMetadata(&item, metadata)
	if items == nil {
		items = make(map[string]SteamMod)
	}
	items[item.ID] = item
	return items, nil
}
