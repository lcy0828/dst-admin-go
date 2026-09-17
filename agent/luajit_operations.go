package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"dont/internal/luajit"
	"dont/shared"
)

var luaJITDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var luaJITVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func validateLuaJITPayload(r shared.RuntimeOperationRequest) error {
	if r.Console != nil || r.Logs != nil || r.ChatLogs != nil || r.Artifacts != nil || r.Observation != nil || r.Migration != nil || r.Backup != nil || r.Mod != nil || r.GameVersion != nil || r.Network != nil || r.CPU != nil || r.Configuration != nil || r.Map != nil {
		return errors.New("LuaJIT 请求包含无关负载")
	}
	if r.Action == shared.RuntimeActionLuaJITObserve {
		if r.LuaJIT != nil && *r.LuaJIT != (shared.RuntimeLuaJITRequest{RefreshCatalog: true}) {
			return errors.New("LuaJIT 检测请求不能携带安装参数")
		}
		return nil
	}
	p := r.LuaJIT
	if p == nil || p.RefreshCatalog {
		return errors.New("LuaJIT 请求缺少参数")
	}
	if r.Action == shared.RuntimeActionLuaJITDownload {
		if p.ReleaseID != "" || p.Release != nil || p.DownloadPath != "" || p.DownloadToken != "" {
			return errors.New("LuaJIT 下载请求包含安装参数")
		}
		return luajit.ValidateSource(p.SourceURL, p.SHA256)
	}
	if r.Action != shared.RuntimeActionLuaJITInstall || p.SourceURL != "" || p.SHA256 != "" {
		return errors.New("LuaJIT 安装请求无效")
	}
	if p.Release == nil {
		if !luaJITDigest.MatchString(p.ReleaseID) || p.DownloadPath != "" || p.DownloadToken != "" {
			return errors.New("请选择运行节点上的 LuaJIT 安装包")
		}
		return nil
	}
	release := p.Release
	if p.ReleaseID != "" || !luaJITDigest.MatchString(release.ID) || release.SHA256 != release.ID || release.Size < 1 || release.Size > luajit.MaxPackageBytes || release.OS != "linux" || release.Arch != "amd64" || !luaJITVersion.MatchString(release.Version) || p.DownloadPath != "/luajit-packages/"+release.ID || !luaJITDigest.MatchString(p.DownloadToken) {
		return errors.New("LuaJIT 控制端传包请求无效")
	}
	return nil
}

// A node owns one persistent package cache, shared by its installations.
func (a *Agent) luaJITPackages() (*luajit.Store, error) {
	a.luaJITMu.Lock()
	defer a.luaJITMu.Unlock()
	if a.luaJITStore != nil {
		return a.luaJITStore, nil
	}
	directory, err := luajit.DefaultReleaseDir()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(os.Getenv("DST_ADMIN_LUAJIT_RELEASE_DIR")) == "" && a.Config != nil && a.Config.OperationStateFile != "" {
		directory = filepath.Join(filepath.Dir(a.Config.OperationStateFile), "luajit-releases")
	}
	a.luaJITStore, err = luajit.NewStore(directory)
	return a.luaJITStore, err
}

func (a *Agent) executeLuaJITAction(ctx context.Context, installation RuntimeInstallation, r shared.RuntimeOperationRequest) (shared.RuntimeOperationResult, error) {
	result := runtimeResult(r, shared.RuntimeOutcomeObserved, "LuaJIT 安装状态已读取")
	options := luajit.Options{ServerPath: installation.ServerPath, ServerMode: installation.ServerMode}
	store, err := a.luaJITPackages()
	if err != nil {
		return result, err
	}
	if r.Action == shared.RuntimeActionLuaJITObserve {
		if r.LuaJIT != nil && r.LuaJIT.RefreshCatalog {
			if err := store.RefreshUpstream(ctx); err != nil {
				return result, err
			}
		}
		value := options.Inspect()
		result.LuaJIT = &value
		result.LuaJITReleases, err = store.Available()
		return result, err
	}
	if installation.Driver == "container" {
		return result, errors.New("独立分片容器暂不支持在线安装 LuaJIT，请使用 Native 或 All-in-One Runtime")
	}
	if !options.Supported() {
		return result, luajit.ErrUnsupported
	}
	if r.Action == shared.RuntimeActionLuaJITDownload {
		release, err := store.ImportURL(ctx, r.LuaJIT.SourceURL, r.LuaJIT.SHA256)
		if err != nil {
			return result, err
		}
		result.LuaJITRelease = &release
		result.Outcome, result.Message = shared.RuntimeOutcomeConfirmed, "运行节点上的 LuaJIT 安装包已就绪"
		return result, nil
	}
	if err := options.CheckStopped(ctx); err != nil {
		return result, err
	}
	var release shared.LuaJITRelease
	if r.LuaJIT.Release != nil {
		release, err = a.receiveLuaJITPackage(ctx, store, r.LuaJIT)
	} else {
		release, err = store.EnsureAvailable(ctx, r.LuaJIT.ReleaseID)
	}
	if err != nil {
		return result, err
	}
	archive, err := store.Path(release.ID)
	if err != nil {
		return result, err
	}
	report, err := luajit.Install(ctx, options, release, archive)
	result.LuaJIT, result.LuaJITRelease = &report, &release
	if err != nil {
		result.Outcome, result.Message = shared.RuntimeOutcomeFailed, err.Error()
		return result, err
	}
	a.sendActiveReport()
	result.Outcome, result.Message = shared.RuntimeOutcomeConfirmed, "LuaJIT 安装完成"
	return result, nil
}

func (a *Agent) receiveLuaJITPackage(ctx context.Context, store *luajit.Store, p *shared.RuntimeLuaJITRequest) (shared.LuaJITRelease, error) {
	rawURL, err := resolveControllerDownloadURL(a.Config.ServerURL, p.DownloadPath, "/luajit-packages/")
	if err != nil {
		return shared.LuaJITRelease{}, err
	}
	directory, err := os.MkdirTemp("", ".luajit-transfer-*")
	if err != nil {
		return shared.LuaJITRelease{}, err
	}
	defer os.RemoveAll(directory)
	archive, err := luajit.Download(ctx, rawURL, p.DownloadToken, directory, *p.Release)
	if err != nil {
		return shared.LuaJITRelease{}, err
	}
	f, err := os.Open(archive)
	if err != nil {
		return shared.LuaJITRelease{}, err
	}
	defer f.Close()
	release, err := store.Save(ctx, f, p.Release.SHA256)
	if err == nil && release != *p.Release {
		return release, luajit.ErrInvalidPackage
	}
	return release, err
}
