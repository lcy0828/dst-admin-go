package httpapi

import (
	"context"
	"dont/shared"
	"errors"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"testing"
)

type artworkReader struct {
	data        []byte
	err         error
	room, world string
	input       shared.RuntimeEntityArtworkRequest
	calls       int
}

func (r *artworkReader) ReadEntityArtwork(_ context.Context, room, world string, input shared.RuntimeEntityArtworkRequest) ([]byte, error) {
	r.calls++
	r.room, r.world, r.input = room, world, input
	return r.data, r.err
}
func TestArtworkHTTPReturnsPrivateImagesMissingAndNodeErrors(t *testing.T) {
	router := gin.New()
	h := NewEntityCatalogHandler(entityCatalogFixture{})
	reader := &artworkReader{data: []byte("PNG")}
	h.ConfigureArtwork(reader)
	h.Register(router.Group("/api/v2"))
	url := "/api/v2/rooms/room/worlds/caves/entity-catalog/artwork/log?modId=workshop-123"
	req := httptest.NewRequest("GET", url, nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != 200 || response.Header().Get("Content-Type") != "image/png" || response.Header().Get("Cache-Control") != "private, max-age=300" || reader.world != "caves" || reader.input.ModID != "workshop-123" {
		t.Fatal(response, reader)
	}
	etag := response.Header().Get("ETag")
	req.Header.Set("If-None-Match", etag)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusNotModified || response.Body.Len() != 0 {
		t.Fatal(response)
	}
	reader.data = nil
	response = httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != 204 {
		t.Fatal(response)
	}
	reader.err = errors.New("Agent offline")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != 503 || response.Header().Get("Cache-Control") != "" {
		t.Fatal(response)
	}
	before := reader.calls
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", url+"/../secret", nil))
	if response.Code != 422 || reader.calls != before {
		t.Fatal("invalid input sent to node", response)
	}
}
