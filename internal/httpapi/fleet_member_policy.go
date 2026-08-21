package httpapi

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// FleetMemberPolicy prevents a managed node's local UI from competing with
// its upstream controller. Read-only diagnostics and the settings needed to
// leave the Fleet remain available.
func FleetMemberPolicy(memberEnabled bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !memberEnabled || c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead || c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		path := c.Request.URL.Path
		if path == "/api/v2/auth" || strings.HasPrefix(path, "/api/v2/auth/") ||
			path == "/api/v2/system/settings/preview" || path == "/api/v2/system/settings/actions/apply" {
			c.Next()
			return
		}
		Failure(c, http.StatusLocked, "FLEET_MEMBER_READ_ONLY", "该节点已加入集中管理；请在管理中心执行变更", nil)
		c.Abort()
	}
}
