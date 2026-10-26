package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"dont/internal/agents"
	"dont/internal/runtimedriver"
	"dont/internal/topology"
	"dont/shared"
	"github.com/gin-gonic/gin"
)

type inventoryHandlerService struct {
	playerHandlerService
	err error
	q   *shared.PlayerInventoryRequest
}

func (s inventoryHandlerService) Inventory(_ context.Context, _, _ string, q shared.PlayerInventoryRequest) (shared.PlayerInventory, error) {
	if s.q != nil {
		*s.q = q
	}
	return shared.PlayerInventory{SchemaVersion: 1, PlayerID: q.PlayerID, Source: "save", Equipment: []shared.InventoryItem{}, Inventory: []shared.InventoryItem{}, Status: "complete", Empty: true}, s.err
}
func TestInventoryHTTPSourceNullabilityAndErrors(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"empty", nil, 200, ""},
		{"ambiguous", &shared.InventoryError{Code: "SOURCE_AMBIGUOUS", Candidates: []shared.InventoryCandidate{{WorldID: "master", SourceID: "opaque"}}}, 409, "SOURCE_AMBIGUOUS"},
		{"busy", &shared.InventoryError{Code: "INVENTORY_BUSY"}, 429, "INVENTORY_BUSY"},
		{"old agent", runtimedriver.ErrCapabilityMissing, 503, "INVENTORY_CAPABILITY_UNAVAILABLE"},
		{"offline", agents.ErrAgentOffline, 503, "TARGET_OFFLINE"},
		{"placement offline", &topology.ExecutionError{Code: "APPLIED_TARGET_OFFLINE"}, 503, "TARGET_OFFLINE"},
		{"invalid", &shared.InventoryError{Code: "SAVE_INVALID"}, 422, "SAVE_INVALID"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var q shared.PlayerInventoryRequest
			app := gin.New()
			NewPlayerHandler(inventoryHandlerService{err: tt.err, q: &q}, nil).Register(app.Group("/api/v2"))
			response := performJSON(app, http.MethodGet, "/api/v2/rooms/room/players/KU_01234567/inventory?source=save&refresh=true", nil, nil, "")
			assertStatus(t, response, tt.status)
			if tt.code != "" {
				var envelope struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != tt.code {
					t.Fatal(response.Body.String(), err)
				}
			}
			if response.Header().Get("Cache-Control") != "no-store" || q.Source != "save" || !q.Refresh {
				t.Fatal(response.Header(), q)
			}
			if tt.status == 200 {
				data := responseData(t, response)
				if data["savedAt"] != nil || data["slotCount"] != nil || data["activeItem"] != nil || data["empty"] != true {
					t.Fatal(data)
				}
			}
			if tt.status == 429 && response.Header().Get("Retry-After") != "2" {
				t.Fatal(response.Header())
			}
		})
	}
}
