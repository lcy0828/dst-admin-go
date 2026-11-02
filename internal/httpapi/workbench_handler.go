package httpapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"strconv"
	"time"

	"dont/internal/aicontrol"
	"dont/internal/authn"
	consoleapi "dont/internal/console"
	"github.com/gin-gonic/gin"
)

func workbenchCallerContext(c *gin.Context) context.Context {
	ctx := c.Request.Context()
	value, _ := c.Get(authn.ContextAdminKey)
	admin, ok := value.(authn.Admin)
	if !ok || admin.ID < 1 {
		return ctx
	}
	// RequireSession has already verified these credentials. Hash rather than
	// retaining a credential in a command, receipt or Runtime preview.
	identity := fmt.Sprintf("admin:%d", admin.ID)
	if header := c.GetHeader("Authorization"); header != "" {
		identity += fmt.Sprintf(":token:%x", sha256.Sum256([]byte(header)))
	}
	return consoleapi.WithWorkbenchCaller(ctx, identity)
}

func (h *ConsoleHandler) workbench(c *gin.Context) {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 8193))
	if err != nil || len(raw) > 8192 {
		consoleFailure(c, consoleapi.ErrInvalidArguments)
		return
	}
	input, err := aicontrol.StrictBotJSON(raw)
	if err != nil {
		consoleFailure(c, consoleapi.ErrInvalidArguments)
		return
	}
	for key := range input {
		if key != "commandId" && key != "worldId" && key != "arguments" && key != "confirmed" {
			consoleFailure(c, consoleapi.ErrInvalidArguments)
			return
		}
	}
	id, ok := input["commandId"].(string)
	if !ok {
		consoleFailure(c, consoleapi.ErrInvalidArguments)
		return
	}
	args, ok := input["arguments"].(map[string]interface{})
	if !ok {
		consoleFailure(c, consoleapi.ErrInvalidArguments)
		return
	}
	confirmed := false
	if value, exists := input["confirmed"]; exists {
		var valid bool
		confirmed, valid = value.(bool)
		if !valid {
			consoleFailure(c, consoleapi.ErrInvalidArguments)
			return
		}
	}
	if _, err = consoleapi.ValidateBuiltinWorkbench(id, args, confirmed); err != nil {
		consoleFailure(c, err)
		return
	}
	world, ok := input["worldId"].(string)
	if input["worldId"] != nil && !ok || len(world) > 255 {
		consoleFailure(c, consoleapi.ErrInvalidArguments)
		return
	}
	ctx, cancel := context.WithTimeout(workbenchCallerContext(c), 25*time.Second)
	defer cancel()
	if player, _ := args["player_id"].(string); player != "" {
		if h.locator == nil {
			Failure(c, 503, "PLAYER_LOCATION_UNAVAILABLE", "无法确认玩家所在世界，未执行", nil)
			return
		}
		current, e := h.locator.OnlineWorld(ctx, c.Param("roomId"), player)
		if e != nil {
			Failure(c, 409, "PLAYER_LOCATION_UNAVAILABLE", "玩家不在线或分片暂不可确认，未执行", nil)
			return
		}
		if world != "" && world != current.ID {
			Failure(c, 409, "PLAYER_WORLD_CHANGED", "玩家所在世界已变化，未执行", nil)
			return
		}
		world = current.ID
	}
	if world == "" {
		Failure(c, 422, "WORLD_REQUIRED", "请先选择目标世界，未执行", nil)
		return
	}
	result, err := h.console.ExecuteWorkbench(ctx, c.Param("roomId"), world, id, args, confirmed)
	if err != nil {
		consoleFailure(c, err)
		return
	}
	Success(c, 201, result)
}

func (h *ConsoleHandler) workbenchRun(c *gin.Context) {
	refresh, err := strconv.ParseBool(c.DefaultQuery("refresh", "false"))
	if err != nil {
		consoleFailure(c, consoleapi.ErrInvalidArguments)
		return
	}
	value, err := h.console.WorkbenchRun(c.Request.Context(), c.Param("roomId"), c.Param("runId"), refresh)
	if err != nil {
		consoleFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, 200, value)
}
