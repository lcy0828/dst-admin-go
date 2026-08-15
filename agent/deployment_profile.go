package agent

import (
	"errors"
	"os"
	"runtime"
	"strings"
)

const nativeHostIntegrationUnavailable = "NATIVE_HOST_INTEGRATION_UNAVAILABLE"

func agentDeploymentProfile() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DST_ADMIN_AGENT_DEPLOYMENT"))) {
	case "container":
		return "container"
	case "native":
		return "native"
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "container"
	}
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
			value := strings.ToLower(string(data))
			if strings.Contains(value, "docker") || strings.Contains(value, "kubepods") || strings.Contains(value, "containerd") || strings.Contains(value, "libpod") {
				return "container"
			}
		}
	}
	return "native"
}

func validateRuntimeDeployment(installation RuntimeInstallation) error {
	if installation.Driver == "native" && agentDeploymentProfile() == "container" {
		return errors.New(nativeHostIntegrationUnavailable + ": 容器 Agent 不会把 bind mount 路径或 host PID 可见性当作已验证的裸机控制能力；请使用裸机 Agent 或 container Runtime")
	}
	return nil
}

func runtimeDeploymentCapabilities(installations []RuntimeInstallation) (capabilities []string, profiles []string, issues []string) {
	deployment := agentDeploymentProfile()
	capabilities = append(capabilities, "provider.deployment."+deployment+".v1")
	seen := make(map[string]bool)
	for _, installation := range installations {
		profile := installation.Driver
		if profile == "native" && deployment == "container" {
			issue := nativeHostIntegrationUnavailable + ":" + installation.ID
			if !seen[issue] {
				issues = append(issues, issue)
				seen[issue] = true
			}
			continue
		}
		if !seen[profile] {
			profiles = append(profiles, profile)
			capabilities = append(capabilities, "runtime.execution."+profile+".v1")
			if profile == "container" {
				capabilities = append(capabilities, "provider.container-runtime.v1", "runtime.container.v1")
			}
			seen[profile] = true
		}
	}
	return capabilities, profiles, issues
}
