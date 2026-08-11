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
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const (
	steamResponseLimit      = int64(16 * 1024 * 1024)
	steamCommunityPageSize  = 30
	steamCommunityUserAgent = "dst-admin-go/1.0 (+https://github.com/lcy0828/dst-admin-go)"
)

type MetadataProvider interface {
	Search(context.Context, string, int, int) (SearchResult, error)
	Details(context.Context, []string) (map[string]SteamMod, error)
}

type SteamProvider struct {
	APIKey        string
	AppID         string
	HTTPClient    *http.Client
	APIBase       string
	CommunityBase string
}

func NewSteamProvider(apiKey, appID string) *SteamProvider {
	if strings.TrimSpace(appID) == "" {
		appID = "322330"
	}
	return &SteamProvider{
		APIKey: strings.TrimSpace(apiKey), AppID: appID,
		HTTPClient: &http.Client{Timeout: 12 * time.Second}, APIBase: "https://api.steampowered.com",
		CommunityBase: "https://steamcommunity.com",
	}
}

func (p *SteamProvider) Search(ctx context.Context, query string, page, pageSize int) (SearchResult, error) {
	query = strings.TrimSpace(query)
	if err := validateSearch(query, page, pageSize); err != nil {
		return SearchResult{}, err
	}
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
		return p.searchCommunity(ctx, query, page, pageSize)
	}
	parameters := url.Values{
		"key": {p.APIKey}, "appid": {p.AppID}, "search_text": {query}, "page": {strconv.Itoa(page)},
		"numperpage": {strconv.Itoa(pageSize)}, "language": {"6"}, "return_tags": {"true"},
		"return_vote_data": {"true"}, "return_children": {"true"},
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
	return SearchResult{Items: items, Total: payload.Response.Total, Page: page, PageSize: pageSize}, nil
}

func (p *SteamProvider) searchCommunity(ctx context.Context, query string, page, pageSize int) (SearchResult, error) {
	start := (page - 1) * pageSize
	communityPage := start/steamCommunityPageSize + 1
	offset := start % steamCommunityPageSize
	pageCount := (offset + pageSize + steamCommunityPageSize - 1) / steamCommunityPageSize
	items := make([]SteamMod, 0, pageCount*steamCommunityPageSize)
	seen := make(map[string]bool, cap(items))
	total := -1
	for index := 0; index < pageCount; index++ {
		batch, batchTotal, err := p.searchCommunityPage(ctx, query, communityPage+index)
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
			if detail.Name == "" {
				detail.Name = item.Name
			}
			if detail.PreviewURL == "" {
				detail.PreviewURL = item.PreviewURL
			}
			items[index] = detail
		}
	}
	return SearchResult{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

func (p *SteamProvider) searchCommunityPage(ctx context.Context, query string, page int) ([]SteamMod, int, error) {
	parameters := url.Values{
		"appid": {p.AppID}, "searchtext": {query}, "browsesort": {"textsearch"},
		"section": {"items"}, "p": {strconv.Itoa(page)}, "l": {"english"},
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
		items = append(items, SteamMod{ID: id, Name: strings.TrimSpace(name), PreviewURL: previewURL})
	})
	total := -1
	document.Find("div").EachWithBreak(func(_ int, selection *goquery.Selection) bool {
		text := strings.TrimSpace(selection.Text())
		const suffix = " entries matching filters"
		if !strings.HasSuffix(text, suffix) {
			return true
		}
		value := strings.TrimSpace(strings.TrimSuffix(text, suffix))
		if parsed, parseErr := strconv.Atoi(value); parseErr == nil && parsed >= 0 {
			total = parsed
			return false
		}
		return true
	})
	return items, total, nil
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
	for _, item := range items {
		result[item.ID] = item
	}
	return result, nil
}

type steamDetailWire struct {
	ID              string  `json:"publishedfileid"`
	Result          int     `json:"result"`
	Creator         string  `json:"creator"`
	ConsumerAppID   int     `json:"consumer_app_id"`
	Title           string  `json:"title"`
	Description     string  `json:"description"`
	FileDescription string  `json:"file_description"`
	PreviewURL      string  `json:"preview_url"`
	Updated         int64   `json:"time_updated"`
	Subscriptions   int64   `json:"subscriptions"`
	Score           float64 `json:"score"`
	VoteData        struct {
		Score float64 `json:"score"`
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
	for _, tag := range value.Tags {
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
		PreviewURL: value.PreviewURL, Subscriptions: value.Subscriptions, Score: score,
		Dependencies: uniqueModIDs(dependencies), Tags: tags,
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

func validateSearch(query string, page, pageSize int) error {
	fields := make(map[string]string)
	if query == "" || len([]rune(query)) > 100 {
		fields["query"] = "搜索内容必须为 1-100 个字符"
	}
	if page < 1 {
		fields["page"] = "页码必须大于 0"
	}
	if pageSize < 1 || pageSize > 50 {
		fields["pageSize"] = "每页数量必须在 1-50 之间"
	}
	if len(fields) > 0 {
		return &FieldError{Fields: fields}
	}
	return nil
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
