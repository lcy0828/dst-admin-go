package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"dont/internal/chatlogs"

	"github.com/gin-gonic/gin"
)

type chatLogHandlerService struct{}

func (chatLogHandlerService) List(_ context.Context, _ string, filter chatlogs.Filter) (chatlogs.List, error) {
	if filter.Kind == chatlogs.Kind("invalid") {
		return chatlogs.List{}, chatlogs.ErrInvalidFilter
	}
	startedAt := time.Date(2026, time.August, 21, 1, 0, 0, 0, time.UTC)
	occurredAt := startedAt.Add(time.Second)
	return chatlogs.List{
		Items: []chatlogs.Entry{{ID: "message", Kind: chatlogs.KindSay, PlayerName: "Willow", Content: "hello", SourceTimestamp: "00:00:01", OccurredAt: &occurredAt}},
		Total: 1, Counts: map[chatlogs.Kind]int{chatlogs.KindSay: 1}, Limit: filter.Limit, Offset: filter.Offset,
		StartedAt: &startedAt,
	}, nil
}

func TestChatLogHTTPListAndValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewChatLogHandler(chatLogHandlerService{}).Register(router.Group("/api/v2"))

	response := performJSON(router, http.MethodGet, "/api/v2/rooms/room/chat-logs?kind=say&limit=50", nil, nil, "")
	assertStatus(t, response, http.StatusOK)
	data := responseData(t, response)
	if data["total"] != float64(1) || data["limit"] != float64(50) {
		t.Fatalf("unexpected chat log list: %s", response.Body.String())
	}
	if data["startedAt"] != "2026-08-21T01:00:00Z" {
		t.Fatalf("missing chat startup time: %s", response.Body.String())
	}

	response = performJSON(router, http.MethodGet, "/api/v2/rooms/room/chat-logs?kind=invalid", nil, nil, "")
	assertAPIError(t, response, http.StatusUnprocessableEntity, "INVALID_CHAT_LOG_FILTER")
}
