package routers

import (
	"path/filepath"
	"strings"

	"dont/internal/deploymentprofile"
	"dont/pkg/setting"
)

func configuredDeploymentProfile(backupPath string) (deploymentprofile.Profile, error) {
	return deploymentProfileFromSnapshot(setting.CurrentSnapshot(), backupPath)
}

func deploymentProfileFromSnapshot(config setting.Snapshot, backupPath string) (deploymentprofile.Profile, error) {
	statePath := config.Path("fleet", "STATE_PATH", "DST_ADMIN_FLEET_STATE_PATH")
	if statePath == "" && strings.TrimSpace(backupPath) != "" {
		statePath = filepath.Join(backupPath, ".dst-admin-fleet")
	}
	return deploymentprofile.Resolve(deploymentprofile.Values{
		Packaging:            config.String("deployment", "PACKAGING", "DST_ADMIN_PACKAGING"),
		LocalExecutorEnabled: config.String("fleet", "LOCAL_EXECUTOR_ENABLED", "DST_ADMIN_LOCAL_EXECUTOR_ENABLED"),
		ControllerEnabled:    config.String("fleet", "CONTROLLER_ENABLED", "DST_ADMIN_FLEET_CONTROLLER_ENABLED"),
		MemberEnabled:        config.String("fleet", "MEMBER_ENABLED", "DST_ADMIN_FLEET_MEMBER_ENABLED"),
		ControllerURL:        config.String("agent", "SERVER_URL", "DST_ADMIN_FLEET_CONTROLLER_URL"),
		MemberKey:            config.String("agent", "SECURITY_KEY", "DST_ADMIN_FLEET_MEMBER_KEY"),
		NodeID:               config.String("fleet", "NODE_ID", "DST_ADMIN_FLEET_NODE_ID"), StatePath: statePath,
	})
}
