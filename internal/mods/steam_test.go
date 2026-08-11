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
			requestedPages = append(requestedPages, page)
			start := 1
			if page == "2" {
				start = 31
			}
			_, _ = fmt.Fprint(writer, `<html><body><div>61 entries matching filters</div>`)
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
	result, err := provider.Search(context.Background(), "棱镜", 2, 20)
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
	if result.Items[0].Name != "Detail 100000021" {
		t.Fatalf("public details were not merged: %#v", result.Items[0])
	}
}

func TestSteamDetailsUsesKeyedEndpointAndRequestsDependencies(t *testing.T) {
	var capturedPath string
	var capturedForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		capturedPath = request.URL.Path
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		capturedForm = request.PostForm
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, `{"response":{"publishedfiledetails":[{"publishedfileid":"378160973","result":1,"consumer_app_id":322330,"title":"Global Positions","children":[{"publishedfileid":"123456789"}]}]}}`)
	}))
	defer server.Close()

	provider := NewSteamProvider("secret-key", "322330")
	provider.APIBase = server.URL
	items, err := provider.Details(context.Background(), []string{"378160973"})
	if err != nil {
		t.Fatal(err)
	}
	if capturedPath != "/IPublishedFileService/GetDetails/v1/" {
		t.Fatalf("path = %q", capturedPath)
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
