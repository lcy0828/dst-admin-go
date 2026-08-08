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
		Failure(c, http.StatusConflict, "REMOTE_RUNTIME_ACTION_UNAVAILABLE", "所选远程节点尚未开放此领域操作", map[string]string{"targetId": targetID})
		c.Abort()
	}
}
