package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"dont/internal/dstruntime"
	"dont/internal/entitycatalog"
	"github.com/gin-gonic/gin"
)

type largeCatalogCommander struct{ calls int }

func (f *largeCatalogCommander) ExecuteCommand(_ context.Context, _, _ string, request dstruntime.CommandRequest) (dstruntime.CommandReceipt, error) {
	f.calls++
	const total = 22000
	offset, limit := request.Arguments["offset"].(int), request.Arguments["limit"].(int)
	items := []entitycatalog.Entity{}
	for i := offset; i < min(offset+limit, total); i++ {
		items = append(items, entitycatalog.Entity{ID: fmt.Sprintf("prefab_%05d", i)})
	}
	return dstruntime.CommandReceipt{OK: true, Details: map[string]any{
		"items": items, "mods": []any{}, "total": total, "offset": offset,
		"hasMore": offset+len(items) < total, "observedAt": "2026-10-25T01:00:00Z",
	}}, nil
}

func TestEntityDirectoriesDefaultToPagesWithinAIToolBudget(t *testing.T) {
	commander := &largeCatalogCommander{}
	h := NewEntityCatalogHandler(entityCatalogFixture{})
	h.ConfigureRuntime(commander)
	router := gin.New()
	h.Register(router.Group("/api/v2"))
	for _, uri := range []string{"/api/v2/entity-catalog/directory", "/api/v2/rooms/room/worlds/Master/entity-catalog/snapshot"} {
		writer := &aiResponseWriter{header: http.Header{}, status: 200}
		router.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, uri, nil))
		if writer.overflow || writer.status != 200 {
			t.Fatalf("AI could not read first page: %s, %d, overflow=%v", uri, writer.status, writer.overflow)
		}
		var result struct {
			Data struct {
				Items   []entitycatalog.Entity `json:"items"`
				Total   int                    `json:"total"`
				HasMore bool                   `json:"hasMore"`
			} `json:"data"`
		}
		if err := json.Unmarshal(writer.body.Bytes(), &result); err != nil || len(result.Data.Items) != 60 || !result.Data.HasMore || result.Data.Total < 6000 {
			t.Fatalf("page=%+v error=%v", result, err)
		}
	}
	calls := commander.calls
	response := performJSON(router, http.MethodGet, "/api/v2/rooms/room/worlds/Master/entity-catalog/snapshot?offset=60&limit=2&observedAt=2026-10-25T01:00:00Z", nil, nil, "")
	data := responseData(t, response)
	if response.Code != 200 || commander.calls != calls || data["offset"] != float64(60) || len(data["items"].([]any)) != 2 {
		t.Fatal("page change collected game metadata again", response.Code, commander.calls, data)
	}
	response = performJSON(router, http.MethodGet, "/api/v2/rooms/room/worlds/Master/entity-catalog/snapshot?offset=60&observedAt=2026-10-24T01:00:00Z", nil, nil, "")
	if response.Code != 409 || commander.calls != calls {
		t.Fatal("changed snapshot was silently mixed into the next page", response.Code, commander.calls)
	}
	response = performJSON(router, http.MethodGet, "/api/v2/entity-catalog/directory?q=goldnugget&limit=1", nil, nil, "")
	data = responseData(t, response)
	if data["total"] != float64(2) || data["hasMore"] != true {
		t.Fatal("filter total is not the matching count", data)
	}
	for _, query := range []string{"limit=0", "limit=121", "offset=-1", "offset=50001", "complete=bad", "complete=true&offset=1", "refresh=true&offset=1", "observedAt=invalid"} {
		response = performJSON(router, http.MethodGet, "/api/v2/rooms/room/worlds/Master/entity-catalog/snapshot?"+query, nil, nil, "")
		if response.Code != 400 || commander.calls != calls {
			t.Fatal("invalid page reached game", query, response.Code, commander.calls)
		}
	}
}
