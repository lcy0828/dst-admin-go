package mods

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"dont/internal/requesttiming"
)

func TestSteamTimingSeparatesHeadersBodyAndJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Error("unexpected Steam request method")
		}
		fmt.Fprint(w, `{"response":{"publishedfiledetails":[{"publishedfileid":"100","result":1,"consumer_app_id":322330,"title":"Test Mod"}]}}`)
	}))
	defer server.Close()
	provider := NewSteamProvider("", "322330")
	provider.APIBase = server.URL
	ctx, timing := requesttiming.New(context.Background())
	items, err := provider.Summaries(ctx, []string{"100"})
	if err != nil || items["100"].Name != "Test Mod" {
		t.Fatalf("timing changed metadata: %#v, %v", items, err)
	}
	_, metrics := timing.Snapshot()
	if len(metrics) != 3 {
		t.Fatalf("unexpected HTTP stages: %#v", metrics)
	}
	for index, name := range []string{"steam.decode_json", "steam.response_body", "steam.response_headers"} {
		if metrics[index].Name != name || metrics[index].Calls != 1 {
			t.Fatalf("missing HTTP measurement: %#v", metrics)
		}
	}
}
