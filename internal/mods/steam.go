package mods

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"dont/internal/requesttiming"

	"github.com/PuerkitoBio/goquery"
)

const (
	steamResponseLimit      = int64(16 * 1024 * 1024)
	steamCommunityPageSize  = 30
	steamCommunityUserAgent = "dst-admin-go/1.0 (+https://github.com/lcy0828/dst-admin-go)"
	steamCommunityCacheTTL  = 7 * 24 * time.Hour
	steamCommunityWorkers   = 2
)

var steamRatingImagePattern = regexp.MustCompile(`(?:^|/)([1-5])-star_large(?:[.?]|$)`)

type MetadataProvider interface {
	Search(context.Context, SearchOptions) (SearchResult, error)
	Details(context.Context, []string) (map[string]SteamMod, error)
}

type SteamProvider struct {
	APIKey            string
	AppID             string
	HTTPClient        *http.Client
	APIBase           string
	CommunityBase     string
	cacheMu           sync.Mutex
	communityCache    map[string]communityMetadataCache
	communityCalls    map[string]*communityMetadataCall
	communitySlots    chan struct{}
	communityCooldown time.Time
	store             *MetadataStore
	summarySlot       chan struct{}
}

type communityMetadataCall struct {
	done     chan struct{}
	metadata communityMetadataCache
	ok       bool
}

type communityMetadataCache struct {
	Name        string
	Author      string
	Description string
	Score       float64
	RatingCount int64
	ExpiresAt   time.Time
	Err         error `json:"-"`
}

type steamInt64 int64

func (value *steamInt64) UnmarshalJSON(data []byte) error {
	text := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if text == "" || text == "null" {
		*value = 0
		return nil
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return err
	}
	*value = steamInt64(parsed)
	return nil
}

func NewSteamProvider(apiKey, appID string) *SteamProvider {
	if strings.TrimSpace(appID) == "" {
		appID = "322330"
	}
	return &SteamProvider{
		APIKey: strings.TrimSpace(apiKey), AppID: appID,
		HTTPClient: &http.Client{Timeout: 12 * time.Second}, APIBase: "https://api.steampowered.com",
		CommunityBase: "https://steamcommunity.com", communityCache: make(map[string]communityMetadataCache),
		communityCalls: make(map[string]*communityMetadataCall), communitySlots: make(chan struct{}, steamCommunityWorkers),
		summarySlot: make(chan struct{}, 1),
	}
}

func (p *SteamProvider) Search(ctx context.Context, options SearchOptions) (SearchResult, error) {
	options = normalizeSearchOptions(options)
	if err := validateSearch(options); err != nil {
		return SearchResult{}, err
	}
	query, page, pageSize := options.Query, options.Page, options.PageSize
	if validModID(query) {
		details, err := p.Details(ctx, []string{query})
		if err != nil && len(details) == 0 {
			return SearchResult{}, err
		}
		items := []SteamMod{}
		if item, ok := details[query]; ok {
			items = append(items, item)
		}
		result := SearchResult{Items: items, Total: len(items), Page: 1, PageSize: pageSize}
		if err != nil {
			result.Warning = err.Error()
		} else if len(items) > 0 && strings.TrimSpace(items[0].Author) == "" {
			result.Warning = "工坊暂未返回作者资料，已保留现有信息；60 秒后可重试"
		}
		return result, nil
	}
	if p.APIKey == "" {
		return p.searchCommunity(ctx, options)
	}
	parameters := url.Values{
		"key": {p.APIKey}, "appid": {p.AppID}, "search_text": {query}, "page": {strconv.Itoa(page)},
		"numperpage": {strconv.Itoa(pageSize)}, "language": {"6"}, "return_tags": {"true"},
		"return_vote_data": {"true"}, "return_children": {"true"},
		"query_type": {steamQueryType(options.Sort)}, "days": {strconv.Itoa(options.Days)},
	}
	for index, tag := range options.Tags {
		parameters.Set(fmt.Sprintf("requiredtags[%d]", index), tag)
	}
	endpoint := strings.TrimRight(p.APIBase, "/") + "/IPublishedFileService/QueryFiles/v1/?" + parameters.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return SearchResult{}, err
	}
	var payload struct {
		Response struct {
			Total int               `json:"total"`
			Items []steamDetailWire `json:"publishedfiledetails"`
		} `json:"response"`
	}
	if err := p.doJSON(request, &payload); err != nil {
		return SearchResult{}, fmt.Errorf("search Steam Workshop: %w", err)
	}
	items := make([]SteamMod, 0, len(payload.Response.Items))
	for _, item := range payload.Response.Items {
		if converted, ok := item.mod(p.AppID); ok {
			items = append(items, converted)
		}
	}
	p.populateAuthors(ctx, items)
	p.rememberSearch(items, displayLanguage)
	return SearchResult{Items: items, Total: payload.Response.Total, Page: page, PageSize: pageSize}, nil
}

func (p *SteamProvider) searchCommunity(ctx context.Context, options SearchOptions) (SearchResult, error) {
	page, pageSize := options.Page, options.PageSize
	start := (page - 1) * pageSize
	communityPage := start/steamCommunityPageSize + 1
	offset := start % steamCommunityPageSize
	pageCount := (offset + pageSize + steamCommunityPageSize - 1) / steamCommunityPageSize
	items := make([]SteamMod, 0, pageCount*steamCommunityPageSize)
	seen := make(map[string]bool, cap(items))
	total := -1
	for index := 0; index < pageCount; index++ {
		batch, batchTotal, err := p.searchCommunityPage(ctx, options, communityPage+index)
		if err != nil {
			return SearchResult{}, err
		}
		if batchTotal >= 0 {
			total = batchTotal
		}
		for _, item := range batch {
			if !seen[item.ID] {
				seen[item.ID] = true
				items = append(items, item)
			}
		}
		if len(batch) < steamCommunityPageSize {
			break
		}
	}
	if total < 0 {
		total = (communityPage-1)*steamCommunityPageSize + len(items)
		if len(items) == pageCount*steamCommunityPageSize {
			total++
		}
	}
	if offset >= len(items) {
		return SearchResult{Items: []SteamMod{}, Total: total, Page: page, PageSize: pageSize}, nil
	}
	end := min(offset+pageSize, len(items))
	items = items[offset:end]
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	details, err := p.Summaries(ctx, ids)
	if err != nil {
		return SearchResult{}, fmt.Errorf("load public Steam Workshop search details: %w", err)
	}
	for index, item := range items {
		if detail, ok := details[item.ID]; ok {
			if item.Name != "" {
				detail.Name = item.Name
			}
			if item.PreviewURL != "" {
				detail.PreviewURL = item.PreviewURL
			}
			if item.Author != "" {
				detail.Author = item.Author
			}
			if item.AuthorID != "" {
				detail.AuthorID = item.AuthorID
			}
			items[index] = detail
		}
	}
	p.rememberSearch(items, steamCommunityLanguage(options.Query))
	return SearchResult{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

func (p *SteamProvider) searchCommunityPage(ctx context.Context, options SearchOptions, page int) ([]SteamMod, int, error) {
	select {
	case p.communitySlots <- struct{}{}:
		defer func() { <-p.communitySlots }()
	case <-ctx.Done():
		return nil, -1, ctx.Err()
	}
	if err := p.communityLimit(); err != nil {
		return nil, -1, err
	}
	parameters := url.Values{
		"appid": {p.AppID}, "searchtext": {options.Query}, "browsesort": {steamCommunitySort(options.Sort)},
		"section": {"readytouseitems"}, "p": {strconv.Itoa(page)},
		"num_per_page": {strconv.Itoa(steamCommunityPageSize)}, "days": {strconv.Itoa(options.Days)},
		"l": {steamCommunityLanguage(options.Query)},
	}
	for _, tag := range options.Tags {
		parameters.Add("requiredtags[]", tag)
	}
	endpoint := strings.TrimRight(p.CommunityBase, "/") + "/workshop/browse/?" + parameters.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, -1, err
	}
	request.Header.Set("User-Agent", steamCommunityUserAgent)
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	request.Header.Set("Accept", "text/html")
	response, err := p.HTTPClient.Do(request)
	if err != nil {
		return nil, -1, fmt.Errorf("search public Steam Workshop: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if response.StatusCode == http.StatusTooManyRequests {
			p.limitCommunity(response.Header.Get("Retry-After"))
			return nil, -1, p.communityLimit()
		}
		return nil, -1, fmt.Errorf("public Steam Workshop returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, steamResponseLimit+1))
	if err != nil {
		return nil, -1, err
	}
	if int64(len(data)) > steamResponseLimit {
		return nil, -1, fmt.Errorf("public Steam Workshop response exceeds %d bytes", steamResponseLimit)
	}
	document, err := goquery.NewDocumentFromReader(bytes.NewReader(data))
	if err != nil {
		return nil, -1, fmt.Errorf("parse public Steam Workshop response: %w", err)
	}
	if body := strings.ToLower(document.Find("body").Text()); strings.Contains(body, "too many requests") || strings.Contains(body, "请求太多") {
		p.limitCommunity(response.Header.Get("Retry-After"))
		return nil, -1, p.communityLimit()
	}
	items := make([]SteamMod, 0, steamCommunityPageSize)
	seen := make(map[string]bool, steamCommunityPageSize)
	document.Find(`a[href*="/sharedfiles/filedetails/"]`).Each(func(_ int, selection *goquery.Selection) {
		image := selection.ChildrenFiltered("img").First()
		if image.Length() == 0 {
			return
		}
		href, exists := selection.Attr("href")
		if !exists {
			return
		}
		parsed, parseErr := url.Parse(href)
		if parseErr != nil || parsed.Path != "/sharedfiles/filedetails/" {
			return
		}
		id := parsed.Query().Get("id")
		if !validModID(id) || seen[id] {
			return
		}
		seen[id] = true
		name, _ := image.Attr("alt")
		previewURL, _ := image.Attr("src")
		item := SteamMod{ID: id, Name: strings.TrimSpace(name), PreviewURL: previewURL}
		for _, ancestor := range selection.Parents().Nodes {
			container := goquery.NewDocumentFromNode(ancestor)
			authorLink := container.Find(`a[href*="/myworkshopfiles/"]`).First()
			if authorLink.Length() == 0 {
				continue
			}
			item.Author = cleanCommunityAuthor(authorLink.Text())
			if authorHref, ok := authorLink.Attr("href"); ok {
				item.AuthorID = communityAuthorID(authorHref)
			}
			break
		}
		items = append(items, item)
	})
	total := -1
	document.Find("div").EachWithBreak(func(_ int, selection *goquery.Selection) bool {
		text := strings.TrimSpace(selection.Text())
		if !strings.HasSuffix(text, " entries matching filters") && !strings.HasSuffix(text, " 个条目符合筛选条件") {
			return true
		}
		value, _, _ := strings.Cut(text, " ")
		if parsed, parseErr := strconv.Atoi(value); parseErr == nil && parsed >= 0 {
			total = parsed
			return false
		}
		return true
	})
	return items, total, nil
}

func steamCommunityLanguage(query string) string {
	for _, character := range query {
		if unicode.Is(unicode.Han, character) {
			return "schinese"
		}
	}
	return "english"
}

func (p *SteamProvider) Details(ctx context.Context, ids []string) (map[string]SteamMod, error) {
	if p.store != nil {
		return p.storedDetails(ctx, ids, true)
	}
	return p.details(ctx, ids, true)
}

// Summaries resolves the batch Workshop fields needed by runtime inventory.
// Per-item community pages are intentionally skipped because they make a
// machine content read scale linearly with the number of installed Mods.
func (p *SteamProvider) Summaries(ctx context.Context, ids []string) (map[string]SteamMod, error) {
	return p.details(ctx, ids, false)
}

func (p *SteamProvider) details(ctx context.Context, ids []string, enrichCommunity bool) (map[string]SteamMod, error) {
	ids = uniqueModIDs(ids)
	if len(ids) == 0 {
		return map[string]SteamMod{}, nil
	}
	if len(ids) > 100 {
		return nil, &FieldError{Fields: map[string]string{"modIds": "一次最多查询 100 个 Mod"}}
	}
	form := url.Values{}
	endpoint := strings.TrimRight(p.APIBase, "/") + "/ISteamRemoteStorage/GetPublishedFileDetails/v1/"
	if p.APIKey == "" {
		form.Set("itemcount", strconv.Itoa(len(ids)))
	} else {
		endpoint = strings.TrimRight(p.APIBase, "/") + "/IPublishedFileService/GetDetails/v1/"
		form.Set("key", p.APIKey)
		form.Set("includechildren", "true")
		form.Set("includetags", "true")
		form.Set("includevotes", "true")
		form.Set("language", "6")
	}
	for index, id := range ids {
		form.Set(fmt.Sprintf("publishedfileids[%d]", index), id)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var payload struct {
		Response struct {
			Items []steamDetailWire `json:"publishedfiledetails"`
		} `json:"response"`
	}
	if err := p.doJSON(request, &payload); err != nil {
		return nil, fmt.Errorf("load Steam Workshop details: %w", err)
	}
	result := make(map[string]SteamMod, len(payload.Response.Items))
	items := make([]SteamMod, 0, len(payload.Response.Items))
	for _, item := range payload.Response.Items {
		converted, ok := item.mod(p.AppID)
		if !ok {
			continue
		}
		items = append(items, converted)
	}
	p.rememberSummaries(items)
	var enrichmentErr error
	if enrichCommunity {
		p.populateAuthors(ctx, items)
		enrichmentErr = p.populateCommunityMetadata(ctx, items)
	} else {
		for index := range items {
			if cached, ok := p.cachedCommunityMetadata(items[index].ID); ok {
				mergeCommunityMetadata(&items[index], cached)
			}
		}
	}
	for _, item := range items {
		result[item.ID] = item
	}
	var missing []string
	for _, id := range ids {
		if _, ok := result[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		enrichmentErr = errors.Join(enrichmentErr, fmt.Errorf("Steam 未返回 Workshop 信息：%s", strings.Join(missing, ", ")))
	}
	return result, enrichmentErr
}

type steamDetailWire struct {
	ID              string     `json:"publishedfileid"`
	Result          int        `json:"result"`
	Creator         string     `json:"creator"`
	ConsumerAppID   int        `json:"consumer_app_id"`
	Title           string     `json:"title"`
	Description     string     `json:"description"`
	FileDescription string     `json:"file_description"`
	PreviewURL      string     `json:"preview_url"`
	Created         int64      `json:"time_created"`
	Updated         int64      `json:"time_updated"`
	Subscriptions   steamInt64 `json:"subscriptions"`
	Favorites       steamInt64 `json:"favorited"`
	Views           steamInt64 `json:"views"`
	FileSize        steamInt64 `json:"file_size"`
	SteamManifestID string     `json:"hcontent_file"`
	Score           float64    `json:"score"`
	VoteData        struct {
		Score     float64    `json:"score"`
		VotesUp   steamInt64 `json:"votes_up"`
		VotesDown steamInt64 `json:"votes_down"`
	} `json:"vote_data"`
	Children []struct {
		ID string `json:"publishedfileid"`
	} `json:"children"`
	Tags []struct {
		Tag         string `json:"tag"`
		DisplayName string `json:"display_name"`
	} `json:"tags"`
}

func (value steamDetailWire) mod(appID string) (SteamMod, bool) {
	if !validModID(value.ID) || (value.Result != 0 && value.Result != 1) {
		return SteamMod{}, false
	}
	if value.ConsumerAppID != 0 && strconv.Itoa(value.ConsumerAppID) != appID {
		return SteamMod{}, false
	}
	description := value.Description
	if description == "" {
		description = value.FileDescription
	}
	dependencies := make([]string, 0, len(value.Children))
	for _, child := range value.Children {
		if validModID(child.ID) {
			dependencies = append(dependencies, child.ID)
		}
	}
	tags := make([]string, 0, len(value.Tags))
	version := ""
	for _, tag := range value.Tags {
		if version == "" {
			version = versionFromTag(tag.Tag)
		}
		name := tag.DisplayName
		if name == "" {
			name = tag.Tag
		}
		if name != "" {
			tags = append(tags, name)
		}
	}
	score := value.Score
	if value.VoteData.Score > 0 {
		score = value.VoteData.Score
	}
	item := SteamMod{
		ID: value.ID, Name: value.Title, AuthorID: value.Creator, Description: description,
		Version: version, PreviewURL: value.PreviewURL, Subscriptions: int64(value.Subscriptions), Score: score,
		RatingCount: int64(value.VoteData.VotesUp + value.VoteData.VotesDown), Favorites: int64(value.Favorites),
		Views: int64(value.Views), FileSize: int64(value.FileSize), SteamManifestID: strings.TrimSpace(value.SteamManifestID),
		Dependencies: uniqueModIDs(dependencies), Tags: tags,
	}
	if value.Created > 0 {
		item.CreatedAt = time.Unix(value.Created, 0).UTC()
	}
	if value.Updated > 0 {
		item.UpdatedAt = time.Unix(value.Updated, 0).UTC()
	}
	return item, true
}

func (p *SteamProvider) populateAuthors(ctx context.Context, items []SteamMod) {
	if p.APIKey == "" || len(items) == 0 {
		return
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item.AuthorID != "" {
			ids = append(ids, item.AuthorID)
		}
	}
	if len(ids) == 0 {
		return
	}
	parameters := url.Values{"key": {p.APIKey}, "steamids": {strings.Join(ids, ",")}}
	endpoint := strings.TrimRight(p.APIBase, "/") + "/ISteamUser/GetPlayerSummaries/v2/?" + parameters.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return
	}
	var payload struct {
		Response struct {
			Players []struct {
				ID   string `json:"steamid"`
				Name string `json:"personaname"`
			} `json:"players"`
		} `json:"response"`
	}
	if p.doJSON(request, &payload) != nil {
		return
	}
	names := make(map[string]string, len(payload.Response.Players))
	for _, player := range payload.Response.Players {
		names[player.ID] = player.Name
	}
	for index := range items {
		items[index].Author = names[items[index].AuthorID]
	}
}

func (p *SteamProvider) populateCommunityMetadata(ctx context.Context, items []SteamMod) error {
	var wait sync.WaitGroup
	var mu sync.Mutex
	var failures []error
	seenErrors := make(map[string]bool)
	for index := range items {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			metadata, ok := p.communityMetadata(ctx, items[index].ID)
			if metadata.Err != nil {
				mu.Lock()
				if message := metadata.Err.Error(); !seenErrors[message] {
					seenErrors[message] = true
					failures = append(failures, metadata.Err)
				}
				mu.Unlock()
			}
			if !ok {
				return
			}
			mergeCommunityMetadata(&items[index], metadata)
		}()
	}
	wait.Wait()
	return errors.Join(failures...)
}

func (p *SteamProvider) loadCommunityMetadata(ctx context.Context, id string) (communityMetadataCache, bool) {
	return p.fetchCommunityMetadata(ctx, id)
}

func (p *SteamProvider) fetchCommunityMetadata(ctx context.Context, id string) (communityMetadataCache, bool) {
	if err := p.communityLimit(); err != nil {
		return communityMetadataCache{Err: err}, false
	}
	parameters := url.Values{"id": {id}, "l": {"schinese"}}
	endpoint := strings.TrimRight(p.CommunityBase, "/") + "/sharedfiles/filedetails/?" + parameters.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return communityMetadataCache{Err: err}, false
	}
	request.Header.Set("User-Agent", steamCommunityUserAgent)
	request.Header.Set("Accept", "text/html")
	response, err := p.HTTPClient.Do(request)
	if err != nil {
		return communityMetadataCache{Err: fmt.Errorf("获取 Workshop %s 工坊页面：%w", id, err)}, false
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if response.StatusCode == http.StatusTooManyRequests {
			p.limitCommunity(response.Header.Get("Retry-After"))
			return communityMetadataCache{Err: p.communityLimit()}, false
		}
		return communityMetadataCache{Err: fmt.Errorf("Workshop %s 工坊页面返回 HTTP %d", id, response.StatusCode)}, false
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, steamResponseLimit+1))
	if err != nil {
		return communityMetadataCache{Err: err}, false
	}
	if int64(len(data)) > steamResponseLimit {
		return communityMetadataCache{Err: fmt.Errorf("Workshop %s 工坊页面超过大小限制", id)}, false
	}
	document, err := goquery.NewDocumentFromReader(bytes.NewReader(data))
	if err != nil {
		return communityMetadataCache{Err: err}, false
	}
	metadata := communityMetadataCache{ExpiresAt: time.Now().Add(steamCommunityCacheTTL)}
	metadata.Name = strings.TrimSpace(document.Find(".workshopItemTitle").First().Text())
	if metadata.Name == "" {
		body := strings.ToLower(document.Find("body").Text())
		if strings.Contains(body, "too many requests") || strings.Contains(body, "请求太多") {
			p.limitCommunity(response.Header.Get("Retry-After"))
			return communityMetadataCache{Err: p.communityLimit()}, false
		}
		return communityMetadataCache{Err: fmt.Errorf("Workshop %s 未返回有效的工坊详情页面", id)}, false
	}
	metadata.Author = cleanCommunityAuthor(document.Find(`a[href*="/myworkshopfiles/"]`).First().Text())
	description := document.Find("#highlightContent").First()
	description.Find("br").Each(func(_ int, selection *goquery.Selection) {
		_ = selection.ReplaceWithHtml("\n")
	})
	description.Find(".bb_h1").Each(func(_ int, selection *goquery.Selection) {
		_ = selection.PrependHtml("\n")
		_ = selection.AppendHtml("\n")
	})
	metadata.Description = strings.TrimSpace(description.Text())
	metadata.RatingCount = parseCommunityCount(document.Find(".numRatings").First().Text())
	document.Find(`img[src*="-star_large"]`).EachWithBreak(func(_ int, selection *goquery.Selection) bool {
		source, _ := selection.Attr("src")
		match := steamRatingImagePattern.FindStringSubmatch(source)
		if len(match) != 2 {
			return true
		}
		stars, err := strconv.Atoi(match[1])
		if err == nil {
			metadata.Score = float64(stars) / 5
		}
		return false
	})
	return metadata, metadata.Name != "" || metadata.Author != "" || metadata.Description != "" || metadata.Score > 0 || metadata.RatingCount > 0
}

func (p *SteamProvider) communityMetadata(ctx context.Context, id string) (communityMetadataCache, bool) {
	cached, cachedOK := p.cachedCommunityMetadata(id)
	// Partial pages remain useful for summaries, but must not turn a metadata
	// retry into a cache hit for the entire display TTL.
	if cachedOK {
		return cached, cached.Name != "" || cached.Author != ""
	}
	p.cacheMu.Lock()
	previous := p.communityCache[id]
	if time.Now().Before(previous.ExpiresAt) {
		p.cacheMu.Unlock()
		return previous, previous.Name != "" || previous.Author != ""
	}
	if call, ok := p.communityCalls[id]; ok {
		p.cacheMu.Unlock()
		select {
		case <-call.done:
			return call.metadata, call.ok
		case <-ctx.Done():
			return communityMetadataCache{Err: ctx.Err()}, false
		}
	}
	call := &communityMetadataCall{done: make(chan struct{})}
	p.communityCalls[id] = call
	p.cacheMu.Unlock()

	select {
	case p.communitySlots <- struct{}{}:
		call.metadata, call.ok = p.loadCommunityMetadata(ctx, id)
		<-p.communitySlots
	case <-ctx.Done():
		call.metadata.Err = ctx.Err()
	}

	if !call.ok && (previous.Name != "" || previous.Author != "") {
		err := call.metadata.Err
		call.metadata, call.ok = previous, true
		call.metadata.Err = err
	}
	if call.ok {
		if call.metadata.Author == "" {
			call.metadata.Author = previous.Author
		}
		if call.metadata.Description == "" {
			call.metadata.Description = previous.Description
		}
		if call.metadata.Author == "" {
			call.metadata.ExpiresAt = time.Now().Add(time.Minute)
		}
	}
	if call.ok && call.metadata.Err == nil && p.store != nil {
		if err := p.store.save(id, displayLanguage, nil, &call.metadata, time.Now()); err != nil {
			log.Printf("[WorkshopMetadata] save display id=%s: %v", id, err)
		}
	}
	p.cacheMu.Lock()
	if call.metadata.Err != nil {
		// Avoid repeated failed page loads as the user changes panels. This
		// short backoff is display-only; version API calls never consult it.
		call.metadata.ExpiresAt = time.Now().Add(time.Minute)
		p.communityCache[id] = call.metadata
	} else if call.ok {
		p.communityCache[id] = call.metadata
	}
	delete(p.communityCalls, id)
	close(call.done)
	p.cacheMu.Unlock()
	return call.metadata, call.ok
}

func (p *SteamProvider) limitCommunity(retryAfter string) {
	until := time.Now().Add(time.Minute)
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds > 0 {
		until = time.Now().Add(time.Duration(seconds) * time.Second)
	} else if date, err := http.ParseTime(retryAfter); err == nil && date.After(until) {
		until = date
	}
	p.cacheMu.Lock()
	if until.After(p.communityCooldown) {
		p.communityCooldown = until
	}
	p.cacheMu.Unlock()
}

func (p *SteamProvider) communityLimit() error {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	if remaining := time.Until(p.communityCooldown); remaining > 0 {
		return fmt.Errorf("Steam 暂时限制工坊资料查询，约 %d 秒后可重试；版本检查不受此限制", int(remaining.Seconds())+1)
	}
	return nil
}

func (p *SteamProvider) cachedCommunityMetadata(id string) (communityMetadataCache, bool) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	value, ok := p.communityCache[id]
	if !ok || time.Now().After(value.ExpiresAt) {
		return communityMetadataCache{}, false
	}
	return value, true
}

func mergeCommunityMetadata(item *SteamMod, metadata communityMetadataCache) {
	if metadata.Name != "" {
		item.Name = metadata.Name
	}
	if metadata.Author != "" {
		item.Author = metadata.Author
	}
	if metadata.Description != "" {
		item.Description = metadata.Description
	}
	if item.Score == 0 {
		item.Score = metadata.Score
	}
	if item.RatingCount == 0 {
		item.RatingCount = metadata.RatingCount
	}
}

func (p *SteamProvider) doJSON(request *http.Request, target interface{}) error {
	finishHTTP := requesttiming.Start(request.Context(), "steam.response_headers")
	response, err := p.HTTPClient.Do(request)
	finishHTTP()
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("Steam API returned HTTP %d", response.StatusCode)
	}
	finishBody := requesttiming.Start(request.Context(), "steam.response_body")
	data, err := io.ReadAll(io.LimitReader(response.Body, steamResponseLimit+1))
	finishBody()
	if err != nil {
		return err
	}
	if int64(len(data)) > steamResponseLimit {
		return fmt.Errorf("Steam API response exceeds %d bytes", steamResponseLimit)
	}
	defer requesttiming.Start(request.Context(), "steam.decode_json")()
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("Steam API response contains trailing JSON")
		}
		return fmt.Errorf("Steam API response contains invalid trailing data: %w", err)
	}
	return nil
}

func normalizeSearchOptions(options SearchOptions) SearchOptions {
	options.Query = strings.TrimSpace(options.Query)
	if options.Sort == "" {
		options.Sort = SearchSortRelevance
	}
	if options.Query == "" && options.Sort == SearchSortRelevance {
		options.Sort = SearchSortTrend
	}
	if options.Days == 0 {
		options.Days = 7
	}
	seen := make(map[string]bool, len(options.Tags))
	tags := make([]string, 0, len(options.Tags))
	for _, tag := range options.Tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag != "" && !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}
	options.Tags = tags
	return options
}

func validateSearch(options SearchOptions) error {
	fields := make(map[string]string)
	if len([]rune(options.Query)) > 100 {
		fields["query"] = "搜索内容不能超过 100 个字符"
	}
	if options.Page < 1 {
		fields["page"] = "页码必须大于 0"
	}
	if options.PageSize < 1 || options.PageSize > 50 {
		fields["pageSize"] = "每页数量必须在 1-50 之间"
	}
	if !validSearchSort(options.Sort) {
		fields["sort"] = "不支持的排序方式"
	}
	if !validSearchDays(options.Days) {
		fields["days"] = "时间范围必须是 -1、1、7、30、90、180 或 365 天"
	}
	for _, tag := range options.Tags {
		if !validWorkshopTag(tag) {
			fields["tags"] = "包含不支持的模组分类"
			break
		}
	}
	if len(fields) > 0 {
		return &FieldError{Fields: fields}
	}
	return nil
}

func validSearchSort(value SearchSort) bool {
	switch value {
	case SearchSortRelevance, SearchSortTrend, SearchSortMostRecent, SearchSortLastUpdated, SearchSortMostSubscribed, SearchSortTopRated:
		return true
	default:
		return false
	}
}

func validSearchDays(value int) bool {
	return value == -1 || value == 1 || value == 7 || value == 30 || value == 90 || value == 180 || value == 365
}

func validWorkshopTag(value string) bool {
	switch value {
	case "character", "item", "pet", "creature", "environment", "interface", "utility", "art", "worldgen", "tweak", "scenario", "language", "other", "tutorial", "client_only_mod", "server_only_mod", "all_clients_require_mod", "server_admin":
		return true
	default:
		return false
	}
}

func steamQueryType(value SearchSort) string {
	switch value {
	case SearchSortTrend:
		return "3"
	case SearchSortMostRecent:
		return "1"
	case SearchSortLastUpdated:
		return "21"
	case SearchSortMostSubscribed:
		return "12"
	case SearchSortTopRated:
		return "0"
	default:
		return "11"
	}
}

func steamCommunitySort(value SearchSort) string {
	switch value {
	case SearchSortTrend:
		return "trend"
	case SearchSortMostRecent:
		return "mostrecent"
	case SearchSortLastUpdated:
		return "lastupdated"
	case SearchSortMostSubscribed:
		return "totaluniquesubscribers"
	case SearchSortTopRated:
		return "toprated"
	default:
		return "textsearch"
	}
}

func versionFromTag(value string) string {
	name, version, found := strings.Cut(strings.TrimSpace(value), ":")
	if !found || !strings.EqualFold(name, "version") {
		return ""
	}
	return strings.TrimSpace(version)
}

func cleanCommunityAuthor(value string) string {
	value = strings.TrimSpace(value)
	for _, prefix := range []string{"创作者：", "创作者:", "Creator:", "Creator："} {
		value = strings.TrimSpace(strings.TrimPrefix(value, prefix))
	}
	for _, suffix := range []string{" 的创意工坊", "的创意工坊", "'s Workshop"} {
		value = strings.TrimSpace(strings.TrimSuffix(value, suffix))
	}
	return value
}

func communityAuthorID(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "profiles" {
		return parts[1]
	}
	return ""
}

func parseCommunityCount(value string) int64 {
	var digits strings.Builder
	started := false
	for _, character := range value {
		if character >= '0' && character <= '9' {
			digits.WriteRune(character)
			started = true
			continue
		}
		if started && (character == ',' || character == '.' || unicode.IsSpace(character)) {
			continue
		}
		if started {
			break
		}
	}
	parsed, _ := strconv.ParseInt(digits.String(), 10, 64)
	return parsed
}

func validModID(value string) bool {
	if value == "" || len(value) > 20 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func ValidID(value string) bool { return validModID(value) }

func uniqueModIDs(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if !validModID(value) || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

var _ MetadataProvider = (*SteamProvider)(nil)
