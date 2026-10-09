package softwareupdate

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

func AgentPlatform(platform string) bool {
	switch platform {
	case "linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64", "windows-amd64":
		return true
	}
	return false
}

func DefaultAgentRoot(configPath string) (string, error) {
	if root := strings.TrimSpace(os.Getenv("DST_ADMIN_AGENT_UPDATE_DIR")); root != "" {
		return filepath.Abs(root)
	}
	if configPath == "" {
		return "", ErrInvalid
	}
	return filepath.Abs(filepath.Join(filepath.Dir(configPath), "software-updates"))
}

// The stable parent uses bounded, authenticated loopback requests on all three
// operating systems. No polling or background release downloads run while idle.
type agentLauncherIPC struct {
	token     string
	mu        sync.Mutex
	addresses map[string]string
	client    *http.Client
}

func loopbackURL(raw, endpoint string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Hostname() != "127.0.0.1" || u.Path != endpoint || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port > 0 && port < 65536
}

func localClient() *http.Client {
	return &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrInvalid }}
}

func (a *agentLauncherIPC) address(boot string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.addresses[boot]
}

func (a *agentLauncherIPC) stop(boot string) error {
	address := strings.TrimSuffix(a.address(boot), "/health") + "/shutdown"
	if !loopbackURL(address, "/shutdown") {
		return ErrInvalid
	}
	request, _ := http.NewRequest(http.MethodPost, address, nil)
	request.Header.Set("Authorization", "Bearer "+a.token)
	response, err := a.client.Do(request)
	if response != nil {
		response.Body.Close()
		if response.StatusCode != http.StatusAccepted {
			return ErrInvalid
		}
	}
	return err
}

func RunAgentSupervisor(ctx context.Context, root, configPath, version string, args []string) error {
	base, err := os.Executable()
	if err != nil {
		return err
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return err
	}
	ipc, channel, address, closeIPC, err := newAgentLauncherIPC()
	if err != nil {
		return err
	}
	defer closeIPC()
	env := environmentWith(os.Environ(), map[string]string{"DST_ADMIN_AGENT_LAUNCHER_URL": address, "DST_ADMIN_AGENT_LAUNCHER_TOKEN": ipc.token, "DST_ADMIN_AGENT_UPDATE_DIR": root})
	s := &supervisor{root: root, base: base, baseVersion: version, platform: runtime.GOOS + "-" + runtime.GOARCH, configPath: configPath, args: args, environment: env, signals: channel, bootTimeout: 60 * time.Second, stableDuration: 10 * time.Second, validate: validateAgentExecutable, kind: "agent", agent: ipc}
	return s.run(ctx)
}

func newAgentLauncherIPC() (*agentLauncherIPC, chan os.Signal, string, func(), error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, "", nil, err
	}
	channel := make(chan os.Signal, 1)
	ipc := &agentLauncherIPC{token: newID(), addresses: map[string]string{}, client: localClient()}
	mux := http.NewServeMux()
	mux.HandleFunc("/apply", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+ipc.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		select {
		case channel <- os.Interrupt:
		default:
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+ipc.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var value struct {
			BootID string `json:"bootId"`
			URL    string `json:"url"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&value) != nil || !releaseIDPattern.MatchString(value.BootID) || !loopbackURL(value.URL, "/health") {
			http.Error(w, "invalid", 400)
			return
		}
		ipc.mu.Lock()
		if len(ipc.addresses) >= 4 {
			ipc.addresses = map[string]string{}
		}
		ipc.addresses[value.BootID] = value.URL
		ipc.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	return ipc, channel, "http://" + listener.Addr().String(), func() { server.Close(); ipc.client.CloseIdleConnections() }, nil
}

func wakeAgentLauncher() error {
	address := os.Getenv("DST_ADMIN_AGENT_LAUNCHER_URL") + "/apply"
	if !ManagedChild() || !loopbackURL(address, "/apply") {
		return ErrUnsupported
	}
	request, _ := http.NewRequest(http.MethodPost, address, nil)
	request.Header.Set("Authorization", "Bearer "+os.Getenv("DST_ADMIN_AGENT_LAUNCHER_TOKEN"))
	client := localClient()
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if response != nil {
		response.Body.Close()
		if response.StatusCode != http.StatusAccepted {
			return fmt.Errorf("Agent launcher rejected restart: HTTP %d", response.StatusCode)
		}
	}
	return err
}

// ServeAgentHealth reports the connection state of this exact child. The
// launcher commits an update only after the new Agent reconnects and stays ready.
func ServeAgentHealth(version string, connected func() bool, shutdown func()) (func(), error) {
	if !ManagedChild() {
		return func() {}, nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	token := os.Getenv("DST_ADMIN_AGENT_LAUNCHER_TOKEN")
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Header().Set("X-DST-Admin-Boot-ID", os.Getenv("DST_ADMIN_BOOT_ID"))
		w.Header().Set("X-DST-Admin-Version", version)
		if !connected() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		go shutdown()
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	data, _ := json.Marshal(map[string]string{"bootId": os.Getenv("DST_ADMIN_BOOT_ID"), "url": "http://" + listener.Addr().String() + "/health"})
	address := os.Getenv("DST_ADMIN_AGENT_LAUNCHER_URL") + "/register"
	if !loopbackURL(address, "/register") {
		server.Close()
		return nil, ErrInvalid
	}
	request, _ := http.NewRequest(http.MethodPost, address, bytes.NewReader(data))
	request.Header.Set("Authorization", "Bearer "+token)
	client := localClient()
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if response != nil {
		response.Body.Close()
		if response.StatusCode != http.StatusAccepted {
			err = ErrInvalid
		}
	}
	if err != nil {
		server.Close()
		return nil, err
	}
	return func() { _ = server.Close() }, nil
}
