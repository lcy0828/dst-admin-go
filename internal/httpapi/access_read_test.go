package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAccessListsHTTPReadsBOMAndIdentifiesMalformedFile(t *testing.T) {
	app := newConfigurationHandlerApp(t)
	path := filepath.Join(app.roomPath, "adminlist.txt")
	if err := os.WriteFile(path, []byte("\ufeffKU_ONE\r\nKU_TWO\r\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	response := performJSON(app.router, http.MethodGet, "/api/v2/rooms/room/access", nil, nil, "")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	if err := os.WriteFile(filepath.Join(app.roomPath, "blocklist.txt"), []byte("KU_OK\nKU_private/value\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	response = performJSON(app.router, http.MethodGet, "/api/v2/rooms/room/access", nil, nil, "")
	body := response.Body.String()
	if response.Code != 422 || !strings.Contains(body, `"code":"ACCESS_LIST_INVALID"`) || !strings.Contains(body, `"file":"blocklist.txt"`) || !strings.Contains(body, `"line":2`) || strings.Contains(body, "private") || strings.Contains(body, app.roomPath) {
		t.Fatal(response.Code, body)
	}
}
