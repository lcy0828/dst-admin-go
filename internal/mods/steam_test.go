package mods

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSteamSearchWithoutKeyUsesPublicWorkshopAndPreservesPagination(t *testing.T) {
	requestedPages := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/workshop/browse/":
			page := request.URL.Query().Get("p")
			for key, expected := range map[string]string{
				"browsesort": "textsearch", "section": "readytouseitems", "num_per_page": "30", "days": "7",
			} {
				if value := request.URL.Query().Get(key); value != expected {
					t.Errorf("community %s = %q, want %q", key, value, expected)
				}
			}
			if language := request.URL.Query().Get("l"); language != "schinese" {
				t.Errorf("community language = %q, want schinese", language)
			}
			requestedPages = append(requestedPages, page)
			start := 1
			if page == "2" {
				start = 31
			}
			_, _ = fmt.Fprint(writer, `<html><body><div>61 个条目符合筛选条件</div>`)
			for index := start; index < start+30; index++ {
				id := fmt.Sprintf("100000%03d", index)
				_, _ = fmt.Fprintf(writer, `<a href="https://steamcommunity.com/sharedfiles/filedetails/?id=%s"><img src="preview-%d" alt="Result %d"></a>`, id, index, index)
			}
			_, _ = fmt.Fprint(writer, `</body></html>`)
		case "/ISteamRemoteStorage/GetPublishedFileDetails/v1/":
			if err := request.ParseForm(); err != nil {
				t.Fatal(err)
			}
			items := make([]map[string]interface{}, 0)
			for index := 0; ; index++ {
				id := request.PostForm.Get(fmt.Sprintf("publishedfileids[%d]", index))
				if id == "" {
					break
				}
				items = append(items, map[string]interface{}{
					"publishedfileid": id, "result": 1, "consumer_app_id": 322330, "title": "Detail " + id,
				})
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]interface{}{"response": map[string]interface{}{"publishedfiledetails": items}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	provider := NewSteamProvider("", "322330")
	provider.APIBase = server.URL
	provider.CommunityBase = server.URL
	result, err := provider.Search(context.Background(), SearchOptions{Query: "棱镜", Page: 2, PageSize: 20})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(requestedPages) != "[1 2]" {
		t.Fatalf("community pages = %v", requestedPages)
	}
	if result.Total != 61 || result.Page != 2 || result.PageSize != 20 || len(result.Items) != 20 {
		t.Fatalf("unexpected pagination: %#v", result)
	}
	if result.Items[0].ID != "100000021" || result.Items[19].ID != "100000040" {
		t.Fatalf("unexpected result range: first=%q last=%q", result.Items[0].ID, result.Items[19].ID)
	}
	if result.Items[0].Name != "Result 21" {
		t.Fatalf("localized search title was not preserved: %#v", result.Items[0])
	}
}

func TestSteamCommunityLanguageFollowsTheSearchText(t *testing.T) {
	if language := steamCommunityLanguage("棱镜"); language != "schinese" {
		t.Fatalf("Chinese search language = %q", language)
	}
	if language := steamCommunityLanguage("Global Positions"); language != "english" {
		t.Fatalf("English search language = %q", language)
	}
}

func TestSteamDetailsUsesKeyedEndpointAndRequestsDependencies(t *testing.T) {
	var capturedForm url.Values
	communityLocalized := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/IPublishedFileService/GetDetails/v1/":
			if err := request.ParseForm(); err != nil {
				t.Fatal(err)
			}
			capturedForm = request.PostForm
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(writer, `{"response":{"publishedfiledetails":[{"publishedfileid":"378160973","result":1,"creator":"76561198246008860","consumer_app_id":322330,"title":"Global Positions","description":"English description","children":[{"publishedfileid":"123456789"}]}]}}`)
		case "/ISteamUser/GetPlayerSummaries/v2/":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(writer, `{"response":{"players":[{"steamid":"76561198246008860","personaname":"Steam Author"}]}}`)
		case "/sharedfiles/filedetails/":
			communityLocalized = request.URL.Query().Get("l") == "schinese"
			_, _ = fmt.Fprint(writer, `<html><body><div class="workshopItemTitle">Global Positions-全球定位</div><div id="highlightContent">中文详情</div></body></html>`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	provider := NewSteamProvider("secret-key", "322330")
	provider.APIBase = server.URL
	provider.CommunityBase = server.URL
	items, err := provider.Details(context.Background(), []string{"378160973"})
	if err != nil {
		t.Fatal(err)
	}
	for key, expected := range map[string]string{
		"key": "secret-key", "includechildren": "true", "includetags": "true", "includevotes": "true",
	} {
		if capturedForm.Get(key) != expected {
			t.Fatalf("%s = %q, want %q", key, capturedForm.Get(key), expected)
		}
	}
	if got := items["378160973"].Dependencies; len(got) != 1 || got[0] != "123456789" {
		t.Fatalf("dependencies = %#v", got)
	}
	if !communityLocalized || items["378160973"].Name != "Global Positions-全球定位" || items["378160973"].Description != "中文详情" {
		t.Fatalf("keyed details did not use the Chinese community description: %#v", items["378160973"])
	}
}

func TestSteamDetailsWithoutKeyUsesPublicEndpoint(t *testing.T) {
	var capturedPath string
	var itemCount string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		capturedPath = request.URL.Path
		_ = request.ParseForm()
		itemCount = request.PostForm.Get("itemcount")
		_, _ = fmt.Fprint(writer, `{"response":{"publishedfiledetails":[]}}`)
	}))
	defer server.Close()

	provider := NewSteamProvider("", "322330")
	provider.APIBase = server.URL
	if _, err := provider.Details(context.Background(), []string{"378160973"}); err != nil {
		t.Fatal(err)
	}
	if capturedPath != "/ISteamRemoteStorage/GetPublishedFileDetails/v1/" || itemCount != "1" {
		t.Fatalf("unexpected public request: path=%q itemcount=%q", capturedPath, itemCount)
	}
}

func TestSteamDetailMapsVersionAndWorkshopStats(t *testing.T) {
	value := steamDetailWire{
		ID: "1392778117", Result: 1, Creator: "76561198246008860", ConsumerAppID: 322330,
		Title: "[DST] Legion", Created: 1527083092, Updated: 1779719397,
		Subscriptions: 750761, Favorites: 39164, Views: 1098171, FileSize: 110354453,
	}
	value.Tags = append(value.Tags, struct {
		Tag         string `json:"tag"`
		DisplayName string `json:"display_name"`
	}{Tag: "version:7.6.5"})
	value.VoteData.Score = 0.96
	value.VoteData.VotesUp = 8071

	item, ok := value.mod("322330")
	if !ok {
		t.Fatal("valid Workshop detail was rejected")
	}
	if item.Version != "7.6.5" || item.FileSize != 110354453 || item.Favorites != 39164 || item.Views != 1098171 {
		t.Fatalf("Workshop metadata was not mapped: %#v", item)
	}
	if item.Score != 0.96 || item.RatingCount != 8071 || item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() {
		t.Fatalf("Workshop dates or rating were not mapped: %#v", item)
	}
}

func TestSteamCommunityParsesAuthorAndRating(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/workshop/browse/":
			_, _ = fmt.Fprint(writer, `<html><body><div class="item"><a href="https://steamcommunity.com/sharedfiles/filedetails/?id=1392778117"><img src="preview" alt="[DST] Legion"></a><div><a href="https://steamcommunity.com/profiles/76561198246008860/myworkshopfiles/?appid=322330">创作者：ti_Tout</a></div></div></body></html>`)
		case "/sharedfiles/filedetails/":
			_, _ = fmt.Fprint(writer, `<html><body><div class="workshopItemTitle">[DST] Legion-棱镜</div><a href="https://steamcommunity.com/profiles/76561198246008860/myworkshopfiles/?appid=322330">ti_Tout 的创意工坊</a><div class="fileRatingDetails"><img src="/public/images/sharedfiles/5-star_large.png?v=2"></div><div class="numRatings">8,071 个评价</div><div id="highlightContent"><div class="bb_h1">棱镜官方群组</div>请加QQ群<br>喜欢潜水的小伙伴</div></body></html>`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	provider := NewSteamProvider("", "322330")
	provider.CommunityBase = server.URL
	items, _, err := provider.searchCommunityPage(context.Background(), normalizeSearchOptions(SearchOptions{Query: "棱镜", Page: 1, PageSize: 20}), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Author != "ti_Tout" || items[0].AuthorID != "76561198246008860" {
		t.Fatalf("community author was not parsed: %#v", items)
	}
	metadata, ok := provider.loadCommunityMetadata(context.Background(), "1392778117")
	if !ok || metadata.Name != "[DST] Legion-棱镜" || metadata.Author != "ti_Tout" || metadata.Score != 1 || metadata.RatingCount != 8071 || !strings.Contains(metadata.Description, "请加QQ群\n喜欢潜水") {
		t.Fatalf("community rating was not parsed: %#v", metadata)
	}
}

func TestSearchOptionsSupportBrowsingAndCategories(t *testing.T) {
	options := normalizeSearchOptions(SearchOptions{Page: 1, PageSize: 30, Tags: []string{" ITEM ", "item"}})
	if options.Sort != SearchSortTrend || options.Days != 7 || fmt.Sprint(options.Tags) != "[item]" {
		t.Fatalf("unexpected normalized browsing options: %#v", options)
	}
	if err := validateSearch(options); err != nil {
		t.Fatal(err)
	}
	options.Tags = []string{"unsupported"}
	if err := validateSearch(options); err == nil {
		t.Fatal("unsupported Workshop category was accepted")
	}
}

func TestCleanCommunityAuthorRemovesLocalizedWorkshopLabels(t *testing.T) {
	for input, expected := range map[string]string{
		"创作者：ti_Tout":        "ti_Tout",
		"ti_Tout 的创意工坊":      "ti_Tout",
		"ti_Tout's Workshop": "ti_Tout",
	} {
		if actual := cleanCommunityAuthor(input); actual != expected {
			t.Fatalf("cleanCommunityAuthor(%q) = %q, want %q", input, actual, expected)
		}
	}
}

func TestSteamJSONRejectsTrailingAndOversizedResponses(t *testing.T) {
	for name, body := range map[string]string{
		"trailing JSON":          `{"response":{}} {"unexpected":true}`,
		"invalid trailing bytes": `{"response":{}} garbage`,
		"oversized":              strings.Repeat(" ", int(steamResponseLimit)+1),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(writer, body)
			}))
			defer server.Close()
			provider := NewSteamProvider("", "322330")
			provider.APIBase = server.URL
			if _, err := provider.Details(context.Background(), []string{"378160973"}); err == nil {
				t.Fatal("unsafe Steam response was accepted")
			}
		})
	}
}
