package routers

import (
	"path/filepath"
	"strings"

	"dont/internal/deploymentprofile"
	"dont/pkg/setting"
)

func configuredDeploymentProfile(backupPath string) (deploymentprofile.Profile, error) {
	statePath := setting.Path("fleet", "STATE_PATH", "DST_ADMIN_FLEET_STATE_PATH")
	if statePath == "" && strings.TrimSpace(backupPath) != "" {
		statePath = filepath.Join(backupPath, ".dst-admin-fleet")
	}
	return deploymentprofile.Resolve(deploymentprofile.Values{
		Packaging:            setting.String("deployment", "PACKAGING", "DST_ADMIN_PACKAGING"),
		LocalExecutorEnabled: setting.String("fleet", "LOCAL_EXECUTOR_ENABLED", "DST_ADMIN_LOCAL_EXECUTOR_ENABLED"),
		ControllerEnabled:    setting.String("fleet", "CONTROLLER_ENABLED", "DST_ADMIN_FLEET_CONTROLLER_ENABLED"),
		MemberEnabled:        setting.String("fleet", "MEMBER_ENABLED", "DST_ADMIN_FLEET_MEMBER_ENABLED"),
		ControllerURL:        setting.String("agent", "SERVER_URL", "DST_ADMIN_FLEET_CONTROLLER_URL"),
		MemberKey:            setting.String("agent", "SECURITY_KEY", "DST_ADMIN_FLEET_MEMBER_KEY"),
		NodeID:               setting.String("fleet", "NODE_ID", "DST_ADMIN_FLEET_NODE_ID"), StatePath: statePath,
	})
}
