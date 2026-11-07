package aicontrol

// Opt-in loopback harness: production discovery/validation/receipts with a
// synthetic game boundary hosted by the Python dialogue runner. No game I/O.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"dont/internal/apicontract"
)

func TestCompanionDialogueServer(t *testing.T) {
	ready := os.Getenv("DST_SCENARIO_READY")
	if ready == "" {
		t.Skip("explicit loopback dialogue harness only")
	}
	catalog, err := apicontract.Default()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.RWMutex
	var current *Service
	stop := make(chan struct{})
	var once sync.Once
	client := &http.Client{Timeout: 15 * time.Second}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/stop":
			once.Do(func() { close(stop) })
			fmt.Fprint(w, `{}`)
		case "/configure":
			var in struct {
				Callback string `json:"callback"`
			}
			if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in) != nil {
				http.Error(w, "invalid", 400)
				return
			}
			u, err := url.Parse(in.Callback)
			if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" {
				http.Error(w, "loopback only", 400)
				return
			}
			s, _ := fixture(t, apiBackendFunc(func(ctx context.Context, _ Principal, op *apicontract.Operation, input apicontract.Input) (json.RawMessage, error) {
				body, _ := json.Marshal(map[string]any{"operationId": op.ID, "input": input, "write": op.ExecutionPolicy().Access == "write"})
				req, err := http.NewRequestWithContext(ctx, "POST", in.Callback, bytes.NewReader(body))
				if err != nil {
					return nil, err
				}
				response, err := client.Do(req)
				if err != nil {
					return nil, err
				}
				defer response.Body.Close()
				var out struct {
					Data       json.RawMessage `json:"data"`
					Error      string          `json:"error"`
					HTTPStatus int             `json:"httpStatus"`
				}
				if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&out); err != nil {
					return nil, err
				}
				if out.Error != "" {
					return nil, &PlatformError{Code: out.Error, Message: out.Error}
				}
				if out.HTTPStatus == 0 {
					out.HTTPStatus = 200
				}
				return json.Marshal(map[string]any{"httpStatus": out.HTTPStatus, "data": out.Data})
			}))
			s.ConfigureAPI(catalog)
			mu.Lock()
			current = s
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"capabilities": BotCapabilities()})
		case "/gateway":
			mu.RLock()
			s := current
			mu.RUnlock()
			if s == nil {
				http.Error(w, "not configured", 503)
				return
			}
			var in GenericInput
			raw, err := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
			if err != nil || StrictGenericJSON(raw) != nil || json.Unmarshal(raw, &in) != nil {
				http.Error(w, "invalid", 400)
				return
			}
			result, err := s.Generic(r.Context(), Principal{ID: "synthetic-dialogue-service"}, in)
			out := map[string]any{"data": result}
			if err != nil {
				out["error"] = err.Error()
			}
			json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := os.WriteFile(ready, []byte(server.URL), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stop:
	case <-time.After(80 * time.Minute):
		t.Fatal("dialogue harness deadline")
	}
}
