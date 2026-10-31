package httpapi

import (
	"context"
	"dont/internal/aicontrol"
	"dont/internal/playerinventory"
	"dont/internal/requesttiming"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	consoleapi "dont/internal/console"
	"dont/internal/rooms"

	"github.com/gin-gonic/gin"
)

type OnlinePlayerLocator interface {
	OnlineWorld(context.Context, string, string) (rooms.World, error)
}
type ConsoleHandler struct {
	console *consoleapi.Service
	locator OnlinePlayerLocator
}

func (h *ConsoleHandler) ConfigurePlayerLocator(locator OnlinePlayerLocator) { h.locator = locator }

func NewConsoleHandler(service *consoleapi.Service) *ConsoleHandler {
	return &ConsoleHandler{console: service}
}

func (h *ConsoleHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/commands", h.definitions)
	v2.POST("/commands", h.createDefinition)
	v2.GET("/commands/:commandId", h.definition)
	v2.PUT("/commands/:commandId", h.updateDefinition)
	v2.DELETE("/commands/:commandId", h.deleteDefinition)
	roomsGroup := v2.Group("/rooms/:roomId")
	roomsGroup.GET("/commands", h.definitions)
	roomsGroup.GET("/command-runs", h.runs)
	roomsGroup.DELETE("/command-runs", h.deleteRuns)
	roomsGroup.GET("/command-runs/:runId", h.run)
	roomsGroup.POST("/workbench/actions/execute", h.workbench)
	roomsGroup.GET("/workbench/runs/:runId", h.workbenchRun)
	roomsGroup.POST("/players/:playerId/actions/give-item", h.giveItem)
	roomsGroup.GET("/players/:playerId/item-grants/:runId", h.itemGrant)
	roomsGroup.POST("/players/:playerId/actions/spawn-entity", h.spawnEntity)
	roomsGroup.GET("/players/:playerId/entity-spawns/:runId", h.entitySpawn)
	worldGroup := roomsGroup.Group("/worlds/:worldId")
	worldGroup.POST("/commands", h.execute)
	worldGroup.POST("/raw-commands", h.executeRaw)
}

func (h *ConsoleHandler) giveItem(c *gin.Context) {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 1025))
	if err != nil || len(raw) > 1024 {
		Failure(c, 422, "INVALID_ITEM_GRANT", "发放参数无效；仅支持已知物品和 1–40 个数量", nil)
		return
	}
	body, err := aicontrol.StrictBotJSON(raw)
	item, _ := body["item"].(string)
	count, numeric := body["count"].(float64)
	if err != nil || len(body) != 2 || !numeric || count < 1 || count > 40 || count != float64(int(count)) {
		Failure(c, 422, "INVALID_ITEM_GRANT", "仅接受 item 和整数 count（1–40）；不接受脚本或命令定义", nil)
		return
	}
	if _, err = consoleapi.ResolveGrantItem(item, int(count)); err != nil {
		Failure(c, 422, "INVALID_ITEM_GRANT", "物品名称不唯一或不在已知原版物品目录中；不能发放生物与建筑", nil)
		return
	}
	if h.locator == nil {
		Failure(c, 503, "PLAYER_LOCATION_UNAVAILABLE", "暂时无法确认玩家所在世界，未发放", nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	world, err := h.locator.OnlineWorld(ctx, c.Param("roomId"), c.Param("playerId"))
	if err != nil {
		code := playerinventory.Code(err)
		if code == "SAVE_INVALID" || code == "" {
			code = "PLAYER_LOCATION_UNAVAILABLE"
		}
		Failure(c, 409, code, "无法确认在线玩家所在世界，未发放", nil)
		return
	}
	result, err := h.console.GiveItem(ctx, c.Param("roomId"), world.ID, c.Param("playerId"), item, int(count))
	if err != nil {
		consoleFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusCreated, result)
}

func (h *ConsoleHandler) itemGrant(c *gin.Context) {
	refresh, err := strconv.ParseBool(c.DefaultQuery("refresh", "false"))
	if err != nil {
		Failure(c, 400, "INVALID_QUERY", "refresh 必须为布尔值", nil)
		return
	}
	result, err := h.console.ItemGrant(c.Request.Context(), c.Param("roomId"), c.Param("playerId"), c.Param("runId"), refresh)
	if err != nil {
		consoleFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, result)
}

func (h *ConsoleHandler) definitions(c *gin.Context) {
	definitions, err := h.console.DefinitionsWithError()
	if err != nil {
		Failure(c, http.StatusInternalServerError, "COMMAND_LIST_FAILED", "读取命令列表失败", nil)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": definitions})
}

func (h *ConsoleHandler) definition(c *gin.Context) {
	definition, err := h.console.Definition(c.Param("commandId"))
	if err != nil {
		consoleFailure(c, err)
		return
	}
	Success(c, http.StatusOK, definition)
}

func (h *ConsoleHandler) createDefinition(c *gin.Context) {
	var definition consoleapi.Definition
	if err := c.ShouldBindJSON(&definition); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的命令定义", nil)
		return
	}
	created, err := h.console.CreateDefinition(definition)
	if err != nil {
		consoleFailure(c, err)
		return
	}
	Success(c, http.StatusCreated, created)
}

func (h *ConsoleHandler) updateDefinition(c *gin.Context) {
	var definition consoleapi.Definition
	if err := c.ShouldBindJSON(&definition); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的命令定义", nil)
		return
	}
	updated, err := h.console.UpdateDefinition(c.Param("commandId"), definition)
	if err != nil {
		consoleFailure(c, err)
		return
	}
	Success(c, http.StatusOK, updated)
}

func (h *ConsoleHandler) deleteDefinition(c *gin.Context) {
	if err := h.console.DeleteDefinition(c.Param("commandId")); err != nil {
		consoleFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *ConsoleHandler) runs(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "25"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	if offset < 0 {
		offset = 0
	}
	items, total, err := h.console.Runs(consoleapi.ListFilter{RoomID: c.Param("roomId"), WorldID: c.Query("worldId"), Limit: limit, Offset: offset})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "COMMAND_HISTORY_FAILED", "读取命令历史失败", nil)
		return
	}
	Success(c, http.StatusOK, gin.H{"items": items, "total": total, "limit": limit, "offset": offset})
}

func (h *ConsoleHandler) run(c *gin.Context) {
	refresh := false
	if value, exists := c.GetQuery("refresh"); exists {
		var err error
		refresh, err = strconv.ParseBool(value)
		if err != nil {
			Failure(c, http.StatusBadRequest, "INVALID_QUERY", "refresh 必须为布尔值", nil)
			return
		}
	}
	result, err := h.console.Run(c.Param("runId"))
	if err != nil {
		consoleFailure(c, err)
		return
	}
	if result.RoomID != c.Param("roomId") {
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "命令记录不存在", nil)
		return
	}
	if refresh {
		result, err = h.console.RefreshRun(c.Request.Context(), c.Param("roomId"), result.ID)
		if err != nil {
			consoleFailure(c, err)
			return
		}
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, result)
}

func (h *ConsoleHandler) deleteRuns(c *gin.Context) {
	deleted, err := h.console.DeleteRuns(consoleapi.ListFilter{RoomID: c.Param("roomId"), WorldID: c.Query("worldId")})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "COMMAND_HISTORY_CLEAR_FAILED", "清理命令历史失败", nil)
		return
	}
	Success(c, http.StatusOK, gin.H{"deleted": deleted})
}

func (h *ConsoleHandler) execute(c *gin.Context) {
	var request consoleapi.ExecuteRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的命令", nil)
		return
	}
	ctx, timing := requesttiming.New(c.Request.Context())
	result, err := h.console.Execute(ctx, c.Param("roomId"), c.Param("worldId"), request)
	finishReadTiming(c, timing, err)
	if err != nil {
		consoleFailure(c, err)
		return
	}
	Success(c, http.StatusCreated, result)
}

func (h *ConsoleHandler) executeRaw(c *gin.Context) {
	var request consoleapi.RawRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的原始命令", nil)
		return
	}
	ctx, timing := requesttiming.New(c.Request.Context())
	result, err := h.console.ExecuteRaw(ctx, c.Param("roomId"), c.Param("worldId"), request)
	finishReadTiming(c, timing, err)
	if err != nil {
		consoleFailure(c, err)
		return
	}
	Success(c, http.StatusCreated, result)
}

func consoleFailure(c *gin.Context, err error) {
	switch {
	case errors.Is(err, consoleapi.ErrCommandNotFound), errors.Is(err, consoleapi.ErrRunNotFound), errors.Is(err, consoleapi.ErrDefinitionNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "命令或命令记录不存在", nil)
	case errors.Is(err, consoleapi.ErrInvalidArguments), errors.Is(err, consoleapi.ErrRawCommandInvalid), errors.Is(err, consoleapi.ErrInvalidDefinition):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_COMMAND", "命令参数无效", nil)
	case errors.Is(err, consoleapi.ErrBuiltinDefinition):
		Failure(c, http.StatusConflict, "BUILTIN_COMMAND_IMMUTABLE", "内置命令不能修改或删除", nil)
	case errors.Is(err, consoleapi.ErrConfirmationNeeded):
		Failure(c, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "请输入完整房间名确认该命令", nil)
	case errors.Is(err, consoleapi.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_UNAVAILABLE", "房间当前不可用，请检查运行节点与拓扑状态", nil)
	case errors.Is(err, rooms.ErrInvalidID), errors.Is(err, rooms.ErrUnsafePath):
		Failure(c, http.StatusBadRequest, "INVALID_RESOURCE_ID", "房间或世界标识无效", nil)
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "房间或世界不存在", nil)
	default:
		Failure(c, http.StatusInternalServerError, "COMMAND_SEND_FAILED", "发送命令失败", nil)
	}
}
