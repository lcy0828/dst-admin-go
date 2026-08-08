package mods

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

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
