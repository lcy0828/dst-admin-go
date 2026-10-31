package httpapi

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"dont/internal/aicontrol"
	consoleapi "dont/internal/console"
	"dont/internal/playerinventory"
	"github.com/gin-gonic/gin"
)

func (h *ConsoleHandler) spawnEntity(c *gin.Context) {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 1025))
	if err != nil || len(raw) > 1024 {
		Failure(c, 422, "INVALID_ENTITY_SPAWN", "仅支持受控生物与1–5只数量", nil)
		return
	}
	body, err := aicontrol.StrictBotJSON(raw)
	entity, _ := body["entity"].(string)
	count, numeric := body["count"].(float64)
	if err != nil || len(body) != 2 || !numeric || count < 1 || count > 5 || count != float64(int(count)) {
		Failure(c, 422, "INVALID_ENTITY_SPAWN", "仅接受entity和整数count（1–5），不接受Lua或命令定义", nil)
		return
	}
	if _, err = consoleapi.ResolveSpawnEntity(entity, int(count)); err != nil {
		Failure(c, 422, "INVALID_ENTITY_SPAWN", "仅支持座狼（狗王）、猎犬、蜘蛛、猪人、兔子、蜜蜂；其他实体需专用能力，不自动改用Lua", nil)
		return
	}
	if h.locator == nil {
		Failure(c, 503, "PLAYER_LOCATION_UNAVAILABLE", "无法确认在线玩家分片，未生成", nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	world, err := h.locator.OnlineWorld(ctx, c.Param("roomId"), c.Param("playerId"))
	if err != nil {
		code := playerinventory.Code(err)
		if code == "" || code == "SAVE_INVALID" {
			code = "PLAYER_LOCATION_UNAVAILABLE"
		}
		Failure(c, 409, code, "无法确认在线玩家所在世界，未生成", nil)
		return
	}
	result, err := h.console.SpawnPlayerEntity(ctx, c.Param("roomId"), world.ID, c.Param("playerId"), entity, int(count))
	if err != nil {
		consoleFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusCreated, result)
}

func (h *ConsoleHandler) entitySpawn(c *gin.Context) {
	refresh, err := strconv.ParseBool(c.DefaultQuery("refresh", "false"))
	if err != nil {
		Failure(c, 400, "INVALID_QUERY", "refresh必须为布尔值", nil)
		return
	}
	result, err := h.console.EntitySpawn(c.Request.Context(), c.Param("roomId"), c.Param("playerId"), c.Param("runId"), refresh)
	if err != nil {
		consoleFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, result)
}
