package agent

import (
	"strings"
	"testing"
)

func hasCapability(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func TestContainerAgentRefusesUnverifiedNativeHostIntegration(t *testing.T) {
	t.Setenv("DST_ADMIN_AGENT_DEPLOYMENT", "container")
	installation := RuntimeInstallation{ID: "native", Driver: "native"}
	if err := validateRuntimeDeployment(installation); err == nil || !strings.Contains(err.Error(), nativeHostIntegrationUnavailable) {
		t.Fatalf("error=%v", err)
	}
	capabilities, profiles, issues := runtimeDeploymentCapabilities([]RuntimeInstallation{installation})
	if hasCapability(capabilities, "runtime.execution.native.v1") || len(profiles) != 0 || len(issues) != 1 || !strings.Contains(issues[0], nativeHostIntegrationUnavailable) {
		t.Fatalf("capabilities=%#v profiles=%#v issues=%#v", capabilities, profiles, issues)
	}
}

func TestContainerRuntimeAndNativeAgentCapabilitiesAreSeparate(t *testing.T) {
	t.Setenv("DST_ADMIN_AGENT_DEPLOYMENT", "container")
	capabilities, profiles, issues := runtimeDeploymentCapabilities([]RuntimeInstallation{{ID: "container", Driver: "container"}})
	for _, expected := range []string{"provider.deployment.container.v1", "provider.container-runtime.v1", "runtime.execution.container.v1"} {
		if !hasCapability(capabilities, expected) {
			t.Fatalf("missing %s in %#v", expected, capabilities)
		}
	}
	if len(profiles) != 1 || profiles[0] != "container" || len(issues) != 0 {
		t.Fatalf("profiles=%#v issues=%#v", profiles, issues)
	}

	t.Setenv("DST_ADMIN_AGENT_DEPLOYMENT", "native")
	capabilities, profiles, issues = runtimeDeploymentCapabilities([]RuntimeInstallation{{ID: "native", Driver: "native"}})
	if !hasCapability(capabilities, "provider.deployment.native.v1") || !hasCapability(capabilities, "runtime.execution.native.v1") || len(profiles) != 1 || len(issues) != 0 {
		t.Fatalf("capabilities=%#v profiles=%#v issues=%#v", capabilities, profiles, issues)
	}
}

func TestRuntimeInstallationReportsExposeTrustedSelectionWithoutControlInternals(t *testing.T) {
	reports := runtimeInstallationReports([]RuntimeInstallation{{
		ID: "container", Driver: "container", SavePath: "/srv/dst/saves", ServerPath: "/srv/dst/server",
		SteamCMDPath: "/usr/games/steamcmd", UGCPath: "/srv/dst/workshop", WorkshopContentPath: "/srv/dst/workshop/content", ServerMode: "64",
		ModCachePath: "/private/cache", ModStatePath: "/private/state", ConsoleSocket: "/run/private.sock", ConsoleSession: "dst",
	}})
	if len(reports) != 1 || reports[0]["id"] != "container" || reports[0]["save_path"] != "/srv/dst/saves" {
		t.Fatalf("reports=%#v", reports)
	}
	for _, internal := range []string{"mod_cache_path", "mod_state_path", "console_socket", "console_session"} {
		if _, exists := reports[0][internal]; exists {
			t.Fatalf("report exposed %s: %#v", internal, reports[0])
		}
	}
}
