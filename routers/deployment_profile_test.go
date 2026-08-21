package routers

import (
	"bytes"
	"encoding/base64"
	"path/filepath"
	"testing"

	"dont/internal/deploymentprofile"
	"dont/pkg/setting"

	"github.com/go-ini/ini"
)

func iniForTest(value string) (*ini.File, error) { return ini.Load([]byte(value)) }

func TestConfiguredDeploymentProfileSeparatesPackagingAndRole(t *testing.T) {
	previous := setting.Cfg
	configuration, err := iniForTest("[deployment]\nPACKAGING = all_in_one\n[fleet]\nLOCAL_EXECUTOR_ENABLED = true\nCONTROLLER_ENABLED = false\nMEMBER_ENABLED = false\n")
	if err != nil {
		t.Fatal(err)
	}
	setting.Cfg = configuration
	t.Cleanup(func() { setting.Cfg = previous })
	profile, err := configuredDeploymentProfile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if profile.Packaging != deploymentprofile.PackagingAllInOne || profile.Role != deploymentprofile.RoleStandalone {
		t.Fatalf("profile = %#v", profile)
	}
}

func TestConfiguredDeploymentProfileSupportsEmbeddedMember(t *testing.T) {
	previous := setting.Cfg
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'m'}, 32))
	root := t.TempDir()
	configuration, err := iniForTest("[fleet]\nCONTROLLER_ENABLED = false\nMEMBER_ENABLED = true\nSTATE_PATH = " + filepath.Join(root, "state") + "\n[agent]\nSERVER_URL = wss://controller.example/agent\nSECURITY_KEY = " + key + "\n")
	if err != nil {
		t.Fatal(err)
	}
	setting.Cfg = configuration
	t.Cleanup(func() { setting.Cfg = previous })
	profile, err := configuredDeploymentProfile(root)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Role != deploymentprofile.RoleManagedWorker || profile.StatePath != filepath.Join(root, "state") {
		t.Fatalf("profile = %#v", profile)
	}
}

func TestConfiguredDeploymentProfileDoesNotReuseGatewayCredentialsForMember(t *testing.T) {
	previous := setting.Cfg
	configuration, err := iniForTest("[fleet]\nCONTROLLER_ENABLED = true\nMEMBER_ENABLED = false\n")
	if err != nil {
		t.Fatal(err)
	}
	setting.Cfg = configuration
	t.Cleanup(func() { setting.Cfg = previous })
	t.Setenv("DST_ADMIN_AGENT_SERVER_URL", "wss://legacy-agent.example/agent")
	t.Setenv("DST_ADMIN_AGENT_SECURITY_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'g'}, 32)))
	profile, err := configuredDeploymentProfile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if profile.MemberKeyConfigured || profile.ControllerURL != "" {
		t.Fatalf("gateway credentials leaked into Fleet member profile: %#v", profile)
	}
}
