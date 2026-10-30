package httpapi

import (
	"dont/internal/aicontrol"
	"dont/internal/apicontract"
	"dont/internal/jobs"
	"github.com/gin-gonic/gin"
	"regexp"
	"strings"
)

const aiScopeKey = "dst.ai.principal"

// AITokenScope protects ordinary REST as well as MCP's internal dispatch. It
// runs before idempotency replay: an old response cannot bypass current scope.
func AITokenScope(store *aicontrol.Store, catalog *apicontract.Catalog, jobService *jobs.Service) gin.HandlerFunc {
	routes := map[string]*apicontract.Operation{}
	parameter := regexp.MustCompile(`\{([^}]+)\}`)
	for _, op := range catalog.Operations() {
		routes[op.Method+" /api/v2"+parameter.ReplaceAllString(op.Path, ":$1")] = op
	}
	return func(c *gin.Context) {
		scheme, secret, ok := strings.Cut(c.GetHeader("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			c.Next()
			return
		}
		p, err := store.Authenticate(secret)
		if err != nil {
			Failure(c, 401, "AI_AUTH_INVALID", "接入密钥无效", nil)
			c.Abort()
			return
		}
		c.Set(aiScopeKey, p)
		if !p.Restricted() {
			c.Next()
			return
		}
		deny := func() {
			Failure(c, 403, "AI_SCOPE_DENIED", "此密钥不允许访问该功能或房间", nil)
			c.Abort()
		}
		// Resource records enforce owner and re-enter this same router for their
		// original operation. No anonymous or cross-owner resource authorization.
		if c.Request.Method == "GET" && c.FullPath() == "/api/v2/system/ai/resources/:resourceId" {
			c.Next()
			return
		}
		op := routes[c.Request.Method+" "+c.FullPath()]
		if op == nil {
			deny()
			return
		}
		room := c.Param("roomId")
		if op.ID == "getJob" {
			if jobService == nil {
				deny()
				return
			}
			job, e := jobService.Get(c.Param("jobId"))
			if e != nil || job.RoomID == "" || !p.AllowsRoom(job.RoomID) {
				deny()
				return
			}
		}
		if !p.AllowsOperation(op.ID, room) {
			deny()
			return
		}
		if (p.Profile == aicontrol.ProfileGameRead || p.Profile == aicontrol.ProfileGameManage) && op.ID == "listRoomPlayers" && c.Query("includeAccessLists") != "false" {
			deny()
			return
		}
		if op.ID == "runRoomAction" && p.Profile == aicontrol.ProfileGameManage {
			switch c.Param("action") {
			case "start", "stop", "restart", "save":
			default:
				deny()
				return
			}
		}
		c.Next()
	}
}
