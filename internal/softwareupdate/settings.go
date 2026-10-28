package softwareupdate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Settings belong to the machine doing the download. ProxyURL is write-only
// over HTTP; on disk it is protected with the same 0600 permissions as state.
type Settings struct {
	ControllerRelay bool   `json:"controllerRelay"`
	Source          string `json:"source"`
	ProxyMode       string `json:"proxyMode"`
	ProxyURL        string `json:"proxyURL,omitempty"`
	ProxyConfigured bool   `json:"proxyConfigured"`
	TimeoutMinutes  int    `json:"timeoutMinutes"`
	BackgroundCheck bool   `json:"backgroundCheck"`
	// Keep the existing field name and saved integer-hour values. Fractional
	// hours allow the UI to configure minutes without rewriting old settings.
	CheckIntervalHours float64 `json:"checkIntervalHours"`
}

func DefaultSettings() Settings {
	return Settings{Source: "auto", ProxyMode: "inherit", TimeoutMinutes: 60, BackgroundCheck: true, CheckIntervalHours: 0.5}
}

func (v Settings) Public() Settings { v.ProxyConfigured = v.ProxyURL != ""; v.ProxyURL = ""; return v }

func validateSettings(v Settings) error {
	if !ValidSource(v.Source) || v.TimeoutMinutes < 10 || v.TimeoutMinutes > 240 || !(v.CheckIntervalHours >= 0.5 && v.CheckIntervalHours <= 168) {
		return ErrInvalid
	}
	switch v.ProxyMode {
	case "inherit", "system", "direct", "custom":
	default:
		return ErrInvalid
	}
	if v.ProxyMode == "custom" {
		u, err := url.Parse(v.ProxyURL)
		if err != nil || u.Hostname() == "" || u.Fragment != "" || u.RawQuery != "" || u.Path != "" && u.Path != "/" || len(v.ProxyURL) > 2048 || strings.ContainsAny(v.ProxyURL, "\r\n\x00") {
			return ErrInvalid
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return ErrInvalid
		}
	}
	return nil
}

func (s *Service) settings() (Settings, error) {
	v := DefaultSettings()
	data, err := regularRead(filepath.Join(s.config.Root, "settings.json"), 8192)
	if os.IsNotExist(err) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	if json.Unmarshal(data, &v) != nil {
		return v, ErrInvalid
	}
	return v, validateSettings(v)
}

func (s *Service) Settings() (Settings, error) { v, err := s.settings(); return v.Public(), err }

func (s *Service) SaveSettings(v Settings) (Settings, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	old, err := s.settings()
	if err != nil {
		return Settings{}, err
	}
	if v.ProxyMode == "custom" && v.ProxyURL == "" {
		v.ProxyURL = old.ProxyURL
	}
	if v.ProxyMode != "custom" {
		v.ProxyURL = ""
	}
	if v.Source == "proxy" {
		v.Source = "mirror"
	}
	if err := validateSettings(v); err != nil {
		return Settings{}, err
	}
	if err := ensureRoot(s.config.Root); err != nil {
		return Settings{}, err
	}
	if err := writeJSON(filepath.Join(s.config.Root, "settings.json"), v); err != nil {
		return Settings{}, err
	}
	s.WakeChecks()
	return v.Public(), nil
}

// ResolveNetwork returns an immutable client/configuration pair for this
// operation. Custom test/relay clients retain their transport and contract.
func (s *Service) ResolveNetwork(source string, agent bool) (ReleaseClient, string, Settings, string, error) {
	v, err := s.settings()
	if err != nil {
		return nil, "", v, "", err
	}
	if source == "" {
		source = v.Source
	}
	if !ValidSource(source) {
		return nil, "", v, "", ErrInvalid
	}
	mode, address := v.ProxyMode, v.ProxyURL
	if mode == "inherit" {
		mode = "system"
		if s.config.Proxy != nil {
			mode, address, err = s.config.Proxy()
			if err != nil {
				return nil, "", v, "", err
			}
		}
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(source+"\x00"+mode+"\x00"+address)))
	client := s.config.Client
	if !s.managedClient && !agent {
		return client, source, v, key, nil
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = 20 * time.Second
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.MaxIdleConnsPerHost = 2
	switch mode {
	case "direct":
		transport.Proxy = nil
	case "system":
		transport.Proxy = http.ProxyFromEnvironment
	case "custom":
		proxyURL, parseErr := url.Parse(address)
		if parseErr != nil || validateSettings(Settings{Source: "auto", ProxyMode: "custom", ProxyURL: address, TimeoutMinutes: 60, CheckIntervalHours: 0.5}) != nil {
			return nil, "", v, "", ErrInvalid
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	default:
		return nil, "", v, "", ErrInvalid
	}
	g := NewGitHubClient(&http.Client{Transport: transport})
	if agent || s.config.Kind == "agent" {
		g.kind = "agent"
	}
	if mode != "direct" && source == "auto" {
		direct := transport.Clone()
		direct.Proxy = nil
		g.direct = &http.Client{Transport: direct, CheckRedirect: g.client.CheckRedirect}
	}
	return g, source, v, key, nil
}

func CloseClient(client ReleaseClient) { closeReleaseClient(client) }
func closeReleaseClient(client ReleaseClient) {
	if g, ok := client.(*GitHubClient); ok {
		g.client.CloseIdleConnections()
		if g.direct != nil {
			g.direct.CloseIdleConnections()
		}
	}
}

// Background work sleeps when there is no active subscription. Channel edits
// and settings edits wake it; all checks share the normal on-demand cache.
func (s *Service) WakeChecks() {
	select {
	case s.checkWake <- struct{}{}:
	default:
	}
}
func (s *Service) RunChecks(ctx context.Context, active func() bool, checkAgents func(context.Context)) {
	for ctx.Err() == nil {
		v, err := s.settings()
		delay := time.Duration(0)
		if err == nil && v.BackgroundCheck && active() {
			_, _ = s.Check(ctx, false, "")
			if ctx.Err() == nil && checkAgents != nil {
				checkAgents(ctx)
			}
			delay = time.Duration(v.CheckIntervalHours * float64(time.Hour))
		} else if err != nil {
			delay = time.Hour
		}
		var timer *time.Timer
		var tick <-chan time.Time
		if delay > 0 {
			timer = time.NewTimer(delay)
			tick = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-s.checkWake:
		case <-tick:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}
