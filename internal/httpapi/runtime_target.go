package httpapi

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const RuntimeTargetHeader = "X-DST-Runtime-Target"

var remoteRuntimeControlPrefixes = []string{
	"/api/v2/agents",
	"/api/v2/auth",
	"/api/v2/jobs",
	"/api/v2/runtime-infrastructure",
	"/api/v2/runtime-targets",
	"/api/v2/system/settings",
}

// RuntimeTargetBoundary prevents a remote selection from falling through to
// controller-local domain services while remote domain transports are added.
func RuntimeTargetBoundary() gin.HandlerFunc {
	return func(c *gin.Context) {
		targetID := strings.TrimSpace(c.GetHeader(RuntimeTargetHeader))
		if targetID == "" || targetID == "local" {
			c.Next()
			return
		}
		if !strings.HasPrefix(targetID, "agent:") || len(strings.TrimPrefix(targetID, "agent:")) == 0 {
			Failure(c, http.StatusUnprocessableEntity, "INVALID_RUNTIME_TARGET", "管理目标格式无效", nil)
			c.Abort()
			return
		}
		for _, prefix := range remoteRuntimeControlPrefixes {
			if c.Request.URL.Path == prefix || strings.HasPrefix(c.Request.URL.Path, prefix+"/") {
				c.Next()
				return
			}
		}
		if isTopologyControlPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		if isPlacementAwareRuntimePath(c.Request.URL.Path) {
			c.Next()
			return
		}
		Failure(c, http.StatusConflict, "REMOTE_RUNTIME_ACTION_UNAVAILABLE", "所选远程节点尚未开放此领域操作", map[string]string{"targetId": targetID})
		c.Abort()
	}
}

func isPlacementAwareRuntimePath(value string) bool {
	if !strings.HasPrefix(value, "/api/v2/rooms/") {
		return false
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(value, "/api/v2/rooms/"), "/"), "/")
	if len(parts) < 2 {
		return false
	}
	if parts[1] == "command-runs" || parts[1] == "structured-logs" || parts[1] == "log-rules" {
		return true
	}
	if len(parts) >= 4 && parts[1] == "worlds" {
		switch parts[3] {
		case "commands", "raw-commands", "logs":
			return true
		}
	}
	return false
}

func isTopologyControlPath(value string) bool {
	if !strings.HasPrefix(value, "/api/v2/rooms/") {
		return false
	}
	relative := strings.Trim(strings.TrimPrefix(value, "/api/v2/rooms/"), "/")
	parts := strings.Split(relative, "/")
	return (len(parts) == 2 && parts[1] == "topology") ||
		(len(parts) == 3 && parts[1] == "topology" && parts[2] == "preview") ||
		(len(parts) == 4 && parts[1] == "topology" && parts[2] == "actions" && parts[3] == "apply")
}
