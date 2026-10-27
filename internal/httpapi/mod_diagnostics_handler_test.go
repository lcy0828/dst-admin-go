package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"dont/internal/moddiagnostics"
	"dont/internal/rooms"
	"github.com/gin-gonic/gin"
)

type diagnosticReader struct {
	room, world string
	err         error
}

func (d *diagnosticReader) Analyze(_ context.Context, room, world string) (moddiagnostics.Report, error) {
	d.room, d.world = room, world
	return moddiagnostics.Report{Status: "insufficient_evidence", Observation: moddiagnostics.Observation{RoomID: room, WorldID: world}, Limitations: []string{"LOG_UNAVAILABLE"}}, d.err
}
func TestModDiagnosticHTTPPreservesScopeAndUnknownEvidence(t *testing.T) {
	d := &diagnosticReader{}
	r := gin.New()
	NewModDiagnosticsHandler(d).Register(r.Group("/api/v2"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v2/rooms/room/worlds/Caves/mod-diagnostics", nil))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || d.room != "room" || d.world != "Caves" || body["data"].(map[string]any)["status"] != "insufficient_evidence" {
		t.Fatal(w.Code, w.Body.String())
	}
	d.err = rooms.ErrWorldNotFound
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v2/rooms/room/worlds/missing/mod-diagnostics", nil))
	if w.Code != 404 {
		t.Fatal(w.Code, w.Body.String())
	}
}
