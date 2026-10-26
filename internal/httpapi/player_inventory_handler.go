package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"dont/internal/agents"
	"dont/internal/playerinventory"
	"dont/internal/runtimedriver"
	"dont/internal/topology"
	"dont/shared"

	"github.com/gin-gonic/gin"
)

type inventoryService interface {
	Inventory(context.Context, string, string, shared.PlayerInventoryRequest) (shared.PlayerInventory, error)
}

func (h *PlayerHandler) inventory(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	service, ok := h.players.(inventoryService)
	if !ok {
		Failure(c, 503, "INVENTORY_CAPABILITY_UNAVAILABLE", "当前服务不支持玩家背包查询", nil)
		return
	}
	refresh, err := strconv.ParseBool(c.DefaultQuery("refresh", "false"))
	if err != nil {
		Failure(c, 400, "INVALID_PLAYER_INPUT", "刷新参数无效", nil)
		return
	}
	value, err := service.Inventory(c.Request.Context(), c.Param("roomId"), c.Query("worldId"), shared.PlayerInventoryRequest{PlayerID: c.Param("playerId"), Source: c.DefaultQuery("source", "auto"), SourceID: c.Query("sourceId"), Refresh: refresh})
	if err == nil {
		Success(c, http.StatusOK, value)
		return
	}
	var typed *shared.InventoryError
	code := playerinventory.Code(err)
	if errors.Is(err, runtimedriver.ErrCapabilityMissing) {
		code = "INVENTORY_CAPABILITY_UNAVAILABLE"
	}
	if errors.Is(err, agents.ErrAgentOffline) || errors.Is(err, agents.ErrUnavailable) {
		code = "TARGET_OFFLINE"
	}
	var placement *topology.ExecutionError
	if errors.As(err, &placement) {
		switch placement.Code {
		case "APPLIED_TARGET_OFFLINE":
			code = "TARGET_OFFLINE"
		case "AGENT_CAPABILITY_MISSING":
			code = "INVENTORY_CAPABILITY_UNAVAILABLE"
		default:
			code = "PLAYER_LOCATION_UNCERTAIN"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		code = "INVENTORY_TIMEOUT"
	}
	status := http.StatusUnprocessableEntity
	switch code {
	case "INVALID_PLAYER_ID":
		status = 400
	case "PLAYER_SNAPSHOT_NOT_FOUND":
		status = 404
	case "SOURCE_AMBIGUOUS", "PLAYER_LOCATION_UNCERTAIN", "PLAYER_NOT_READY", "PLAYER_NOT_ONLINE", "SAVE_CHANGED":
		status = 409
	case "INVENTORY_BUSY":
		status = 429
		c.Header("Retry-After", "2")
	case "TARGET_OFFLINE", "INVENTORY_CAPABILITY_UNAVAILABLE":
		status = 503
	case "INVENTORY_TIMEOUT":
		status = 504
	default:
		if code == "SAVE_INVALID" && !playerinventory.IsError(err) {
			playerFailure(c, err)
			return
		}
	}
	var candidates []shared.InventoryCandidate
	if errors.As(err, &typed) {
		candidates = typed.Candidates
	}
	if len(candidates) == 0 {
		candidates = value.Candidates
	}
	Failure(c, status, code, "无法完成玩家背包读取", gin.H{"candidates": candidates, "retryable": status == 409 || status == 429 || status == 504})
}
