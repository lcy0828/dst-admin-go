package mods

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// NetworkConfig belongs to the Controller, not to a Room or Runtime target.
type NetworkConfig struct {
	APIBase       string `json:"apiBase"`
	CommunityBase string `json:"communityBase"`
	ProxyMode     string `json:"proxyMode"`
	ProxyURL      string `json:"proxyUrl"`
	APIKey        string `json:"apiKey"`
	AppID         string `json:"appId"`
}

func NetworkConfigFromValues(values map[string]string) NetworkConfig {
	return NetworkConfig{APIBase: values["mod.steamAPIBase"], CommunityBase: values["mod.steamCommunityBase"], ProxyMode: values["mod.proxyMode"], ProxyURL: values["mod.proxyURL"], APIKey: values["mod.steamAPIKey"], AppID: values["mod.steamAppID"]}
}

func (c NetworkConfig) Normalize() (NetworkConfig, error) {
	c.APIBase, c.CommunityBase = strings.TrimSpace(c.APIBase), strings.TrimSpace(c.CommunityBase)
	c.ProxyMode, c.ProxyURL = strings.TrimSpace(c.ProxyMode), strings.TrimSpace(c.ProxyURL)
	c.APIKey, c.AppID = strings.TrimSpace(c.APIKey), strings.TrimSpace(c.AppID)
	if c.APIBase == "" {
		c.APIBase = defaultSteamAPIBase
	}
	if c.CommunityBase == "" {
		c.CommunityBase = defaultSteamCommunityBase
	}
	if c.ProxyMode == "" {
		c.ProxyMode = "system"
	}
	if c.AppID == "" {
		c.AppID = "322330"
	}
	if !validModID(c.AppID) {
		return c, fmt.Errorf("invalid Steam App ID")
	}
	for _, base := range []string{c.APIBase, c.CommunityBase} {
		u, err := url.Parse(base)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(base, "\r\n\x00") {
			return c, fmt.Errorf("Steam base address must be an HTTP(S) URL without credentials, query or fragment")
		}
	}
	c.APIBase, c.CommunityBase = strings.TrimRight(c.APIBase, "/"), strings.TrimRight(c.CommunityBase, "/")
	switch c.ProxyMode {
	case "system", "direct":
	case "custom":
		u, err := url.Parse(c.ProxyURL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5h") || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(c.ProxyURL, "\r\n\x00") {
			return c, fmt.Errorf("proxy must be an HTTP(S) or socks5h URL")
		}
	default:
		return c, fmt.Errorf("invalid Workshop proxy mode")
	}
	return c, nil
}

// scope is an internal digest; never return it or credential material to clients.
// Including credentials isolates results visible with different Steam accounts.
func (c NetworkConfig) scope() string {
	if c.ProxyMode != "custom" {
		c.ProxyURL = ""
	}
	raw, _ := json.Marshal(c)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
