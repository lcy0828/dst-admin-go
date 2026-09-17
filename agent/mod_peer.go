package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dont/internal/modartifact"
	"dont/shared"

	"github.com/go-ini/ini"
)

const modPeerGrantTTL = 10 * time.Minute

func loadModPeerConfig(config *Config) error {
	if config == nil {
		return errors.New("Agent 配置缺失")
	}
	if isINIConfigPath(config.KeyFile) {
		if file, err := ini.Load(config.KeyFile); err == nil {
			section := file.Section("agent")
			if strings.TrimSpace(config.ModPeerListenAddr) == "" {
				config.ModPeerListenAddr = section.Key("MOD_PEER_LISTEN_ADDR").String()
			}
			if strings.TrimSpace(config.ModPeerAdvertiseURL) == "" {
				config.ModPeerAdvertiseURL = section.Key("MOD_PEER_ADVERTISE_URL").String()
			}
		}
	}
	if value := strings.TrimSpace(os.Getenv("DST_ADMIN_AGENT_MOD_PEER_LISTEN_ADDR")); value != "" {
		config.ModPeerListenAddr = value
	}
	if value := strings.TrimSpace(os.Getenv("DST_ADMIN_AGENT_MOD_PEER_ADVERTISE_URL")); value != "" {
		config.ModPeerAdvertiseURL = value
	}
	config.ModPeerListenAddr = strings.TrimSpace(config.ModPeerListenAddr)
	config.ModPeerAdvertiseURL = strings.TrimRight(strings.TrimSpace(config.ModPeerAdvertiseURL), "/")
	if config.ModPeerListenAddr == "" && config.ModPeerAdvertiseURL == "" {
		return nil
	}
	if config.ModPeerListenAddr == "" || config.ModPeerAdvertiseURL == "" {
		return errors.New("MOD_PEER_LISTEN_ADDR 与 MOD_PEER_ADVERTISE_URL 必须同时配置")
	}
	parsed, err := url.Parse(config.ModPeerAdvertiseURL)
	if err != nil || parsed.User != nil || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Path != "" {
		return errors.New("MOD_PEER_ADVERTISE_URL 必须是无路径、无凭据的 HTTP(S) 地址")
	}
	return nil
}

func (a *Agent) persistModPeerConfig(section *ini.Section) {
	if a == nil || a.Config == nil || section == nil {
		return
	}
	if value := strings.TrimSpace(a.Config.ModPeerListenAddr); value != "" {
		section.Key("MOD_PEER_LISTEN_ADDR").SetValue(value)
	}
	if value := strings.TrimRight(strings.TrimSpace(a.Config.ModPeerAdvertiseURL), "/"); value != "" {
		section.Key("MOD_PEER_ADVERTISE_URL").SetValue(value)
	}
}

func (a *Agent) modPeerEnabled() bool {
	return a != nil && a.Config != nil && a.Config.ModPeerListenAddr != "" && a.Config.ModPeerAdvertiseURL != ""
}

func (a *Agent) startModPeerServer() error {
	if !a.modPeerEnabled() {
		return nil
	}
	listener, err := net.Listen("tcp", a.Config.ModPeerListenAddr)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           a.modPeerHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	a.modPeerMu.Lock()
	a.modPeerListener, a.modPeerServer = listener, server
	a.modPeerMu.Unlock()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Printf("Runtime Peer HTTP 服务异常退出: %v", serveErr)
		}
	}()
	log.Printf("Runtime Peer HTTP 服务已监听 %s，对外地址 %s", listener.Addr(), a.Config.ModPeerAdvertiseURL)
	return nil
}

func (a *Agent) stopModPeerServer() {
	if a == nil {
		return
	}
	a.modPeerMu.Lock()
	server := a.modPeerServer
	a.modPeerServer, a.modPeerListener = nil, nil
	a.modPeerMu.Unlock()
	if server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		_ = server.Close()
	}
}

func (a *Agent) modPeerHandler() http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			response.Header().Set("Allow", "GET, HEAD")
			http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if strings.HasPrefix(request.URL.Path, "/migration-peer/") {
			a.serveMigrationPeer(response, request)
			return
		}
		if !strings.HasPrefix(request.URL.Path, "/mod-peer/") {
			http.NotFound(response, request)
			return
		}
		parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/mod-peer/"), "/")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			http.NotFound(response, request)
			return
		}
		authorization := strings.Fields(strings.TrimSpace(request.Header.Get("Authorization")))
		if len(authorization) != 2 || !strings.EqualFold(authorization[0], "Bearer") {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		service, err := a.existingModPeerArtifactService(parts[0])
		if err != nil {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		descriptor, file, err := service.Open(parts[1], parts[2], authorization[1])
		if err != nil {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		defer file.Close()
		response.Header().Set("Cache-Control", "private, no-store")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		response.Header().Set("X-DST-Mod-Tree-SHA256", descriptor.TreeSHA256)
		response.Header().Set("X-DST-Mod-Bundle-SHA256", descriptor.SHA256)
		response.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
			"filename": "workshop-" + descriptor.WorkshopID + "-" + descriptor.TreeSHA256 + ".tar",
		}))
		http.ServeContent(response, request, "", descriptor.CreatedAt, file)
	})
}

func (a *Agent) existingModPeerArtifactService(installationID string) (*modartifact.Service, error) {
	a.modPeerMu.Lock()
	defer a.modPeerMu.Unlock()
	service := a.modPeerArtifacts[installationID]
	if service == nil {
		return nil, errors.New("Mod Peer 安装实例未授权")
	}
	return service, nil
}

func (a *Agent) modPeerArtifactService(installation RuntimeInstallation) (*modartifact.Service, error) {
	a.modPeerMu.Lock()
	defer a.modPeerMu.Unlock()
	if existing := a.modPeerArtifacts[installation.ID]; existing != nil {
		return existing, nil
	}
	manager, err := a.modManager(installation)
	if err != nil {
		return nil, err
	}
	service, err := modartifact.NewService(manager, filepath.Join(installation.ModStatePath, "peer-bundles"))
	if err != nil {
		return nil, err
	}
	a.modPeerArtifacts[installation.ID] = service
	return service, nil
}

func (a *Agent) issueModPeerGrant(ctx context.Context, installation RuntimeInstallation, subject, workshopID, treeSHA string) (shared.RuntimeModFetchLocation, error) {
	if !a.modPeerEnabled() {
		return shared.RuntimeModFetchLocation{}, errors.New("Agent 未启用 Mod Peer 服务")
	}
	service, err := a.modPeerArtifactService(installation)
	if err != nil {
		return shared.RuntimeModFetchLocation{}, err
	}
	location, _, err := service.Issue(ctx, subject, workshopID, treeSHA, modPeerGrantTTL)
	if err != nil {
		return shared.RuntimeModFetchLocation{}, err
	}
	path := fmt.Sprintf("/mod-peer/%s/%s/%s", url.PathEscape(installation.ID), workshopID, strings.ToLower(treeSHA))
	location.Source = shared.RuntimeModFetchSourcePeer
	location.DownloadPath = path
	location.DownloadURL = a.Config.ModPeerAdvertiseURL + path
	return location, nil
}
