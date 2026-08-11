package mods

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/PuerkitoBio/goquery"
)

const (
	steamResponseLimit      = int64(16 * 1024 * 1024)
	steamCommunityPageSize  = 30
	steamCommunityUserAgent = "dst-admin-go/1.0 (+https://github.com/lcy0828/dst-admin-go)"
	steamCommunityCacheTTL  = 6 * time.Hour
	steamCommunityWorkers   = 6
	steamCommunityAttempts  = 2
	steamCommunityRetryWait = 150 * time.Millisecond
)

var steamRatingImagePattern = regexp.MustCompile(`(?:^|/)([1-5])-star_large(?:[.?]|$)`)

type MetadataProvider interface {
	Search(context.Context, SearchOptions) (SearchResult, error)
	Details(context.Context, []string) (map[string]SteamMod, error)
}

type SteamProvider struct {
	APIKey         string
	AppID          string
	HTTPClient     *http.Client
	APIBase        string
	CommunityBase  string
	cacheMu        sync.Mutex
	communityCache map[string]communityMetadataCache
	communityCalls map[string]*communityMetadataCall
	communitySlots chan struct{}
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
		if err != nil {
			return SearchResult{}, err
		}
		items := []SteamMod{}
		if item, ok := details[query]; ok {
			items = append(items, item)
		}
		return SearchResult{Items: items, Total: len(items), Page: 1, PageSize: pageSize}, nil
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
	p.populateCommunityMetadata(ctx, items)
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
	details, err := p.Details(ctx, ids)
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
	return SearchResult{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

func (p *SteamProvider) searchCommunityPage(ctx context.Context, options SearchOptions, page int) ([]SteamMod, int, error) {
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
	response, err := p.HTTPClient.Do(request)
	if err != nil {
		return nil, -1, fmt.Errorf("search public Steam Workshop: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
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
	p.populateAuthors(ctx, items)
	p.populateCommunityMetadata(ctx, items)
	for _, item := range items {
		result[item.ID] = item
	}
	return result, nil
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
		Views: int64(value.Views), FileSize: int64(value.FileSize),
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

func (p *SteamProvider) populateCommunityMetadata(ctx context.Context, items []SteamMod) {
	var wait sync.WaitGroup
	for index := range items {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			metadata, ok := p.communityMetadata(ctx, items[index].ID)
			if !ok {
				return
			}
			mergeCommunityMetadata(&items[index], metadata)
		}()
	}
	wait.Wait()
}

func (p *SteamProvider) loadCommunityMetadata(ctx context.Context, id string) (communityMetadataCache, bool) {
	for attempt := 0; attempt < steamCommunityAttempts; attempt++ {
		metadata, ok := p.fetchCommunityMetadata(ctx, id)
		if ok {
			return metadata, true
		}
		if attempt == steamCommunityAttempts-1 {
			break
		}
		timer := time.NewTimer(steamCommunityRetryWait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return communityMetadataCache{}, false
		}
	}
	return communityMetadataCache{}, false
}

func (p *SteamProvider) fetchCommunityMetadata(ctx context.Context, id string) (communityMetadataCache, bool) {
	parameters := url.Values{"id": {id}, "l": {"schinese"}}
	endpoint := strings.TrimRight(p.CommunityBase, "/") + "/sharedfiles/filedetails/?" + parameters.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return communityMetadataCache{}, false
	}
	request.Header.Set("User-Agent", steamCommunityUserAgent)
	response, err := p.HTTPClient.Do(request)
	if err != nil {
		return communityMetadataCache{}, false
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return communityMetadataCache{}, false
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, steamResponseLimit+1))
	if err != nil || int64(len(data)) > steamResponseLimit {
		return communityMetadataCache{}, false
	}
	document, err := goquery.NewDocumentFromReader(bytes.NewReader(data))
	if err != nil {
		return communityMetadataCache{}, false
	}
	metadata := communityMetadataCache{ExpiresAt: time.Now().Add(steamCommunityCacheTTL)}
	metadata.Name = strings.TrimSpace(document.Find(".workshopItemTitle").First().Text())
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
	if cached, ok := p.cachedCommunityMetadata(id); ok {
		return cached, true
	}
	p.cacheMu.Lock()
	if call, ok := p.communityCalls[id]; ok {
		p.cacheMu.Unlock()
		select {
		case <-call.done:
			return call.metadata, call.ok
		case <-ctx.Done():
			return communityMetadataCache{}, false
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
	}

	p.cacheMu.Lock()
	if call.ok {
		p.communityCache[id] = call.metadata
	}
	delete(p.communityCalls, id)
	close(call.done)
	p.cacheMu.Unlock()
	return call.metadata, call.ok
}

func (p *SteamProvider) cachedCommunityMetadata(id string) (communityMetadataCache, bool) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	value, ok := p.communityCache[id]
	if !ok || time.Now().After(value.ExpiresAt) {
		delete(p.communityCache, id)
		return communityMetadataCache{}, false
	}
	return value, true
}

func mergeCommunityMetadata(item *SteamMod, metadata communityMetadataCache) {
	if metadata.Name != "" {
		item.Name = metadata.Name
	}
	if item.Author == "" {
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
	response, err := p.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("Steam API returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, steamResponseLimit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > steamResponseLimit {
		return fmt.Errorf("Steam API response exceeds %d bytes", steamResponseLimit)
	}
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
