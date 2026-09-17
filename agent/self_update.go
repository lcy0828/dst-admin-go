package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"dont/shared"
)

const maxAgentUpgradeBytes int64 = 128 * 1024 * 1024

type agentUpdateProfile struct {
	Mode   string `json:"mode"`
	Reason string `json:"reason,omitempty"`
}

func currentAgentUpdateProfile() agentUpdateProfile {
	if runtime.GOOS == "windows" {
		return agentUpdateProfile{Mode: "unsupported", Reason: "windows_service_helper_required"}
	}
	if agentDeploymentProfile() == "container" {
		return agentUpdateProfile{Mode: "container", Reason: "container_image_required"}
	}
	executable, err := os.Executable()
	if err != nil {
		return agentUpdateProfile{Mode: "migration", Reason: "executable_path_unavailable"}
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err == nil {
		executable = resolved
	}
	clean := filepath.Clean(executable)
	if runtime.GOOS == "linux" && clean == "/var/lib/dst-admin-agent/bin/dst-admin-agent" {
		return agentUpdateProfile{Mode: "self"}
	}
	if runtime.GOOS == "darwin" && strings.HasSuffix(clean, filepath.Join("Library", "Application Support", "DST Admin Agent", "bin", "dst-admin-agent")) {
		return agentUpdateProfile{Mode: "self"}
	}
	return agentUpdateProfile{Mode: "migration", Reason: "writable_private_binary_required"}
}

func (a *Agent) executeAgentUpgrade(request *shared.AgentUpgradeRequest, timeoutSeconds int) (shared.AgentUpgradeResult, error) {
	result := shared.AgentUpgradeResult{ProtocolVersion: shared.AgentUpgradeProtocolVersion, PreviousVersion: AgentVersion, ObservedAt: time.Now().UTC()}
	if request == nil {
		return result, errors.New("Agent 升级请求缺失")
	}
	result.ReleaseID, result.Version = request.ReleaseID, request.Version
	if currentAgentUpdateProfile().Mode != "self" {
		return result, errors.New("当前 Agent 安装方式不支持页面内升级")
	}
	if err := validateAgentUpgradeRequest(*request); err != nil {
		return result, err
	}
	downloadURL, err := resolveAgentUpgradeURL(a.Config.ServerURL, request.DownloadPath)
	if err != nil {
		return result, err
	}
	if timeoutSeconds < 30 || timeoutSeconds > 300 {
		timeoutSeconds = 300
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	executable, err := currentAgentExecutable()
	if err != nil {
		return result, err
	}
	temporary, err := downloadAgentRelease(ctx, downloadURL, request.DownloadToken, filepath.Dir(executable), request.Size, request.SHA256)
	if err != nil {
		return result, err
	}
	defer os.Remove(temporary)
	if err := validateDownloadedAgent(ctx, temporary, request.Version); err != nil {
		return result, err
	}
	if err := replaceAgentExecutable(executable, temporary); err != nil {
		return result, err
	}
	result.RestartRequired = true
	result.ObservedAt = time.Now().UTC()
	return result, nil
}

func validateAgentUpgradeRequest(request shared.AgentUpgradeRequest) error {
	if request.ProtocolVersion != shared.AgentUpgradeProtocolVersion || request.ReleaseID == "" || request.Version == "" ||
		request.OS != runtime.GOOS || request.Arch != runtime.GOARCH ||
		request.DownloadPath != "/agent-updates/"+request.ReleaseID || strings.TrimSpace(request.DownloadToken) == "" ||
		len(request.DownloadToken) > 256 || len(request.SHA256) != 64 || request.Size < 1 || request.Size > maxAgentUpgradeBytes ||
		strings.ContainsAny(request.ReleaseID+request.Version+request.DownloadToken+request.SHA256, "\x00\r\n") {
		return errors.New("Agent 升级参数无效")
	}
	if _, err := hex.DecodeString(request.SHA256); err != nil {
		return errors.New("Agent 升级摘要无效")
	}
	return nil
}

func resolveAgentUpgradeURL(serverURL, downloadPath string) (string, error) {
	return resolveControllerDownloadURL(serverURL, downloadPath, "/agent-updates/")
}

func resolveControllerDownloadURL(serverURL, downloadPath, allowedPrefix string) (string, error) {
	controller, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil || controller.Host == "" || controller.User != nil || (controller.Scheme != "ws" && controller.Scheme != "wss") {
		return "", errors.New("Agent 控制端地址无效")
	}
	reference, err := url.Parse(downloadPath)
	if err != nil || reference.IsAbs() || reference.Host != "" || reference.RawQuery != "" || reference.Fragment != "" ||
		!strings.HasPrefix(reference.Path, allowedPrefix) {
		return "", errors.New("Agent 控制端下载地址无效")
	}
	if controller.Scheme == "wss" {
		controller.Scheme = "https"
	} else {
		controller.Scheme = "http"
	}
	controller.Path, controller.RawPath, controller.RawQuery, controller.Fragment = reference.Path, "", "", ""
	return controller.String(), nil
}

func currentAgentExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("读取 Agent 程序路径: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
		path = resolved
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("Agent 程序路径不是普通文件")
	}
	return filepath.Clean(path), nil
}

func downloadAgentRelease(ctx context.Context, rawURL, token, directory string, expectedSize int64, expectedSHA256 string) (string, error) {
	temporary, err := os.CreateTemp(directory, ".dst-admin-agent-upgrade-*")
	if err != nil {
		return "", fmt.Errorf("创建 Agent 升级临时文件: %w", err)
	}
	path := temporary.Name()
	remove := true
	defer func() {
		_ = temporary.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{
		Timeout: time.Duration(300) * time.Second,
		CheckRedirect: func(next *http.Request, previous []*http.Request) error {
			if len(previous) == 0 || !strings.EqualFold(next.URL.Scheme, previous[0].URL.Scheme) || !strings.EqualFold(next.URL.Host, previous[0].URL.Host) {
				return errors.New("Agent 升级下载不允许跨主机重定向")
			}
			return nil
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("下载 Agent 升级包: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载 Agent 升级包: HTTP %d", response.StatusCode)
	}
	if response.ContentLength >= 0 && response.ContentLength != expectedSize {
		return "", errors.New("Agent 升级包长度与发布记录不一致")
	}
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, digest), io.LimitReader(response.Body, expectedSize+1))
	if err != nil {
		return "", fmt.Errorf("写入 Agent 升级包: %w", err)
	}
	if written != expectedSize || !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), expectedSHA256) {
		return "", errors.New("Agent 升级包完整性校验失败")
	}
	if err := temporary.Chmod(0o700); err != nil {
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	remove = false
	return path, nil
}

func validateDownloadedAgent(ctx context.Context, path, expectedVersion string) error {
	command := exec.CommandContext(ctx, path, "-version")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("验证 Agent 升级包版本: %w", err)
	}
	if strings.TrimSpace(string(output)) != expectedVersion {
		return fmt.Errorf("Agent 升级包版本不匹配: 期望 %s，实际 %s", expectedVersion, strings.TrimSpace(string(output)))
	}
	return nil
}

func replaceAgentExecutable(current, replacement string) error {
	return replaceAgentExecutableWithSync(current, replacement, syncAgentDirectory)
}

func replaceAgentExecutableWithSync(current, replacement string, syncDirectory func(string) error) error {
	backup := current + ".previous"
	_ = os.Remove(backup)
	if err := os.Rename(current, backup); err != nil {
		return fmt.Errorf("备份当前 Agent 程序: %w", err)
	}
	if err := os.Rename(replacement, current); err != nil {
		if restoreErr := os.Rename(backup, current); restoreErr != nil {
			return fmt.Errorf("安装 Agent 程序失败: %v；恢复旧版本也失败: %w", err, restoreErr)
		}
		return fmt.Errorf("安装 Agent 程序: %w", err)
	}
	if err := syncDirectory(filepath.Dir(current)); err != nil {
		if rollbackErr := rollbackAgentExecutable(current, backup, replacement); rollbackErr != nil {
			return fmt.Errorf("同步 Agent 程序目录失败: %v；恢复旧版本也失败: %w", err, rollbackErr)
		}
		_ = syncDirectory(filepath.Dir(current))
		return fmt.Errorf("同步 Agent 程序目录失败，已恢复旧版本: %w", err)
	}
	return nil
}

func rollbackAgentExecutable(current, backup, replacement string) error {
	if err := os.Rename(current, replacement); err != nil {
		return fmt.Errorf("移开新 Agent 程序: %w", err)
	}
	if err := os.Rename(backup, current); err != nil {
		if restoreNewErr := os.Rename(replacement, current); restoreNewErr != nil {
			return fmt.Errorf("恢复旧 Agent 程序: %v；重新放回新程序也失败: %w", err, restoreNewErr)
		}
		return fmt.Errorf("恢复旧 Agent 程序: %w", err)
	}
	_ = os.Remove(replacement)
	return nil
}

func syncAgentDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("同步 Agent 程序目录: %w", err)
	}
	return nil
}

func restartAgentAfterUpgrade() {
	time.Sleep(500 * time.Millisecond)
	os.Exit(75)
}
