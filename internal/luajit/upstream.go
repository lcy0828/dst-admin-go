package luajit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dont/shared"
)

const upstreamReleasesURL = "https://api.github.com/repos/fesily/DontStarveLuaJIT2/releases/latest"

// RefreshUpstream is an explicit node-side metadata request. It never downloads
// package contents or starts a background poller.
func (s *Store) RefreshUpstream(ctx context.Context) error {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	response, err := fetchPublicSource(ctx, upstreamReleasesURL, publicSourceClient(6*time.Second))
	if err != nil {
		return fmt.Errorf("运行节点无法检查 LuaJIT 上游版本: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("检查 LuaJIT 上游版本失败: HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("LuaJIT 上游版本响应过大")
	}
	release, err := upstreamRelease(data)
	if err != nil {
		return err
	}
	output, _ := json.Marshal([]bundledPackage{{Release: release}})
	return writeFile(filepath.Join(s.root, "catalog", "upstream.json"), output, 0600)
}
func upstreamRelease(data []byte) (shared.LuaJITRelease, error) {
	var response struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			Digest string `json:"digest"`
			URL    string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if json.Unmarshal(data, &response) != nil || response.Draft || response.Prerelease {
		return shared.LuaJITRelease{}, ErrInvalidPackage
	}
	version := strings.TrimPrefix(response.Tag, "v")
	if !releaseVersion.MatchString(version) {
		return shared.LuaJITRelease{}, errors.New("上游版本号无效")
	}
	for _, asset := range response.Assets {
		if asset.Name != "linux_Mod.zip" {
			continue
		}
		digest := strings.TrimPrefix(asset.Digest, "sha256:")
		expected := "https://github.com/fesily/DontStarveLuaJIT2/releases/download/" + response.Tag + "/linux_Mod.zip"
		if asset.URL != expected || !packageID.MatchString(digest) || asset.Size < 1 || asset.Size > MaxPackageBytes {
			return shared.LuaJITRelease{}, errors.New("上游 Linux 包缺少有效的来源、大小或 SHA-256")
		}
		return shared.LuaJITRelease{ID: digest, Version: version, OS: "linux", Arch: "amd64", SHA256: digest, Size: asset.Size, Channel: "upstream", SourceURL: asset.URL}, nil
	}
	return shared.LuaJITRelease{}, errors.New("上游发布未提供 Linux 安装包")
}
func (s *Store) packageCatalog() ([]bundledPackage, error) {
	packages, err := bundledPackages()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.root, "catalog", "upstream.json"))
	if os.IsNotExist(err) {
		data, err = bundled.ReadFile("packages/upstream.json")
	}
	if err != nil {
		return nil, err
	}
	var upstream []bundledPackage
	if err = json.Unmarshal(data, &upstream); err != nil {
		return nil, err
	}
	for _, p := range upstream {
		r := p.Release
		base := "https://github.com/fesily/DontStarveLuaJIT2/releases/download/"
		validURL := r.SourceURL == base+"v"+r.Version+"/linux_Mod.zip" || r.SourceURL == base+r.Version+"/linux_Mod.zip"
		if r.Channel != "upstream" || p.File != "" || r.ID != r.SHA256 || !packageID.MatchString(r.ID) || !releaseVersion.MatchString(r.Version) || !validURL || r.Size < 1 || r.Size > MaxPackageBytes {
			return nil, ErrInvalidPackage
		}
	}
	return append(upstream, packages...), nil
}
