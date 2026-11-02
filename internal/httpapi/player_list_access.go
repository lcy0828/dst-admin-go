package httpapi

import (
	"net/http"
	"net/url"
	"strconv"

	"dont/internal/aicontrol"
	"dont/internal/players"
	"github.com/gin-gonic/gin"
)

const playerListAccessKey = "dst.players.access-lists"

// Used by both scope middleware and the handler. Parse once, preserving absent,
// empty and duplicate values; gin.GetQuery conflates the first two.
func playerListAccess(c *gin.Context) (aicontrol.PlayerListAccess, bool) {
	if cached, exists := c.Get(playerListAccessKey); exists {
		return cached.(aicontrol.PlayerListAccess), true
	}
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	var requested *bool
	if values, exists := query["includeAccessLists"]; exists && err == nil {
		if len(values) != 1 {
			err = players.ErrInvalidFilter
		} else {
			var value bool
			value, err = strconv.ParseBool(values[0])
			requested = &value
		}
	}
	if err != nil {
		playerFailure(c, players.ErrInvalidFilter)
		c.Abort()
		return aicontrol.PlayerListAccess{}, false
	}
	var principal aicontrol.Principal
	if trusted, exists := c.Get(aiScopeKey); exists {
		principal = trusted.(aicontrol.Principal)
	}
	access, err := principal.ResolvePlayerListAccess(requested)
	if err != nil {
		Failure(c, http.StatusForbidden, "AI_SCOPE_DENIED", aicontrol.AccessListsPermissionMessage, map[string]any{"reason": "ACCESS_LISTS_PERMISSION_REQUIRED"})
		c.Abort()
		return aicontrol.PlayerListAccess{}, false
	}
	c.Set(playerListAccessKey, access)
	return access, true
}
