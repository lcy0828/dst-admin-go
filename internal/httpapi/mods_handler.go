package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"dont/internal/jobs"
	"dont/internal/mods"
	"dont/internal/rooms"
	"dont/internal/runtimeguard"

	"github.com/gin-gonic/gin"
)

type ModHandler struct {
	mods      modService
	placement modPlacementReader
	jobs      *jobs.Service
}

type modPlacementReader interface {
	RoomList(context.Context, string) (mods.ModList, error)
	ConfigurationFile(context.Context, string, string) (mods.ConfigurationFile, error)
	Configuration(context.Context, string, string, string) (mods.ModConfiguration, error)
	PreviewConfiguration(context.Context, string, string, string, mods.ConfigUpdateRequest) (mods.ConfigPreview, error)
}

type modService interface {
	Search(context.Context, mods.SearchOptions) (mods.SearchResult, error)
	Library(context.Context) (mods.ModList, error)
	List(context.Context, string) (mods.ModList, error)
	Download(context.Context, mods.DownloadRequest, io.Writer) (mods.ActionResult, error)
	AddToRoom(context.Context, string, string, string, mods.AddToRoomRequest) (mods.ActionResult, error)
	UpdateLibrary(context.Context, string, io.Writer) (mods.ActionResult, error)
	Install(context.Context, string, string, mods.InstallRequest, io.Writer) (mods.ActionResult, error)
	Update(context.Context, string, string, io.Writer) (mods.ActionResult, error)
	Enable(context.Context, string, string, string, mods.EnableRequest) (mods.ActionResult, error)
	Repair(context.Context, string, string, mods.ModActionRequest, io.Writer) (mods.ActionResult, error)
	Uninstall(context.Context, string, string, string, mods.ModActionRequest) (mods.ActionResult, error)
	CheckUpdates(context.Context, string) (mods.ActionResult, error)
	ConfigurationFile(string, string) (mods.ConfigurationFile, error)
	Configuration(context.Context, string, string, string) (mods.ModConfiguration, error)
	PreviewConfiguration(context.Context, string, string, string, mods.ConfigUpdateRequest) (mods.ConfigPreview, error)
	ApplyConfiguration(context.Context, string, string, string, string, mods.ConfigUpdateRequest) (mods.ConfigApplyResult, error)
}

func NewModHandler(service modService, jobService *jobs.Service) *ModHandler {
	return &ModHandler{mods: service, jobs: jobService}
}

func (h *ModHandler) ConfigurePlacementReader(reader modPlacementReader) {
	h.placement = reader
}

func (h *ModHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/mods/search", h.search)
	v2.GET("/mods/library", h.library)
	v2.POST("/mods/library/actions/download", h.download)
	v2.POST("/mods/library/:modId/actions/update", h.updateLibrary)
	v2.GET("/mods/:modId", h.details)

	room := v2.Group("/rooms/:roomId")
	room.GET("/mods", h.list)
	room.POST("/mods/:modId/actions/add", h.addToRoom)
	room.POST("/mods/actions/install", h.install)
	room.POST("/mods/actions/check-updates", h.checkUpdates)
	room.POST("/mods/:modId/actions/update", h.update)
	room.POST("/mods/:modId/actions/enable", h.enable)
	room.POST("/mods/:modId/actions/repair", h.repair)
	room.POST("/mods/:modId/actions/uninstall", h.uninstall)
	room.GET("/worlds/:worldId/mods/configuration-file", h.configurationFile)

	configuration := room.Group("/worlds/:worldId/mods/:modId/configuration")
	configuration.GET("", h.configuration)
	configuration.POST("/preview", h.previewConfiguration)
	configuration.POST("/actions/apply", h.applyConfiguration)
}

func (h *ModHandler) library(c *gin.Context) {
	value, err := h.mods.Library(c.Request.Context())
	if err != nil {
		modFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ModHandler) download(c *gin.Context) {
	var request mods.DownloadRequest
	if !bindModJSON(c, &request) {
		return
	}
	if !mods.ValidID(request.ModID) {
		modFailure(c, mods.ErrInvalidModID)
		return
	}
	h.submit(c, "mod.download", "", "", request.ModID, "Workshop "+request.ModID, func(ctx context.Context, _ string) (mods.ActionResult, error) {
		return h.mods.Download(ctx, request, log.Writer())
	})
}

func (h *ModHandler) updateLibrary(c *gin.Context) {
	modID := c.Param("modId")
	if !mods.ValidID(modID) {
		modFailure(c, mods.ErrInvalidModID)
		return
	}
	h.submit(c, "mod.update", "", "", modID, "Workshop "+modID, func(ctx context.Context, _ string) (mods.ActionResult, error) {
		return h.mods.UpdateLibrary(ctx, modID, log.Writer())
	})
}

func (h *ModHandler) addToRoom(c *gin.Context) {
	if h.requirePublication(c) {
		return
	}
	var request mods.AddToRoomRequest
	if !bindModJSON(c, &request) {
		return
	}
	roomID, modID := c.Param("roomId"), c.Param("modId")
	if !mods.ValidID(modID) {
		modFailure(c, mods.ErrInvalidModID)
		return
	}
	h.submit(c, "mod.room.add", roomID, "", modID, "Workshop "+modID, func(ctx context.Context, jobID string) (mods.ActionResult, error) {
		return h.mods.AddToRoom(ctx, jobID, roomID, modID, request)
	})
}

func (h *ModHandler) search(c *gin.Context) {
	options, ok := modSearchOptions(c)
	if !ok {
		return
	}
	value, err := h.mods.Search(c.Request.Context(), options)
	if err != nil {
		modFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ModHandler) details(c *gin.Context) {
	value, err := h.mods.Search(c.Request.Context(), mods.SearchOptions{
		Query: c.Param("modId"), Sort: mods.SearchSortRelevance, Days: 7, Page: 1, PageSize: 1,
	})
	if err != nil {
		modFailure(c, err)
		return
	}
	if len(value.Items) == 0 {
		Failure(c, http.StatusNotFound, "MOD_NOT_FOUND", "Steam Workshop 中不存在该 Mod", nil)
		return
	}
	Success(c, http.StatusOK, value.Items[0])
}

func (h *ModHandler) list(c *gin.Context) {
	var value mods.ModList
	var err error
	if h.placement != nil {
		value, err = h.placement.RoomList(c.Request.Context(), c.Param("roomId"))
	} else {
		value, err = h.mods.List(c.Request.Context(), c.Param("roomId"))
	}
	if err != nil {
		modFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ModHandler) install(c *gin.Context) {
	if h.requirePublication(c) {
		return
	}
	var request mods.InstallRequest
	if !bindModJSON(c, &request) {
		return
	}
	if !mods.ValidID(request.ModID) {
		modFailure(c, mods.ErrInvalidModID)
		return
	}
	roomID := c.Param("roomId")
	h.submit(c, "mod.install", roomID, "", request.ModID, "Workshop "+request.ModID, func(ctx context.Context, jobID string) (mods.ActionResult, error) {
		return h.mods.Install(ctx, jobID, roomID, request, log.Writer())
	})
}

func (h *ModHandler) update(c *gin.Context) {
	if h.requirePublication(c) {
		return
	}
	roomID, modID := c.Param("roomId"), c.Param("modId")
	if !mods.ValidID(modID) {
		modFailure(c, mods.ErrInvalidModID)
		return
	}
	h.submit(c, "mod.update", roomID, "", modID, "Workshop "+modID, func(ctx context.Context, _ string) (mods.ActionResult, error) {
		return h.mods.Update(ctx, roomID, modID, log.Writer())
	})
}

func (h *ModHandler) enable(c *gin.Context) {
	if h.requirePublication(c) {
		return
	}
	var request mods.EnableRequest
	if !bindModJSON(c, &request) {
		return
	}
	roomID, modID := c.Param("roomId"), c.Param("modId")
	if !mods.ValidID(modID) {
		modFailure(c, mods.ErrInvalidModID)
		return
	}
	h.submit(c, "mod.enable", roomID, "", modID, "Workshop "+modID, func(ctx context.Context, jobID string) (mods.ActionResult, error) {
		return h.mods.Enable(ctx, jobID, roomID, modID, request)
	})
}

func (h *ModHandler) repair(c *gin.Context) {
	if h.requirePublication(c) {
		return
	}
	var request mods.ModActionRequest
	if !bindModJSON(c, &request) {
		return
	}
	roomID, modID := c.Param("roomId"), c.Param("modId")
	if !mods.ValidID(modID) {
		modFailure(c, mods.ErrInvalidModID)
		return
	}
	h.submit(c, "mod.repair", roomID, "", modID, "Workshop "+modID, func(ctx context.Context, _ string) (mods.ActionResult, error) {
		return h.mods.Repair(ctx, roomID, modID, request, log.Writer())
	})
}

func (h *ModHandler) uninstall(c *gin.Context) {
	if h.requirePublication(c) {
		return
	}
	var request mods.ModActionRequest
	if !bindModJSON(c, &request) {
		return
	}
	roomID, modID := c.Param("roomId"), c.Param("modId")
	if !mods.ValidID(modID) {
		modFailure(c, mods.ErrInvalidModID)
		return
	}
	h.submit(c, "mod.uninstall", roomID, "", modID, "Workshop "+modID, func(ctx context.Context, jobID string) (mods.ActionResult, error) {
		return h.mods.Uninstall(ctx, jobID, roomID, modID, request)
	})
}

func (h *ModHandler) checkUpdates(c *gin.Context) {
	roomID := c.Param("roomId")
	h.submit(c, "mod.check-updates", roomID, "", roomID, "Mod 更新检查", func(ctx context.Context, _ string) (mods.ActionResult, error) {
		return h.mods.CheckUpdates(ctx, roomID)
	})
}

func (h *ModHandler) configurationFile(c *gin.Context) {
	var value mods.ConfigurationFile
	var err error
	if h.placement != nil {
		value, err = h.placement.ConfigurationFile(c.Request.Context(), c.Param("roomId"), c.Param("worldId"))
	} else {
		value, err = h.mods.ConfigurationFile(c.Param("roomId"), c.Param("worldId"))
	}
	if err != nil {
		modFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ModHandler) configuration(c *gin.Context) {
	var value mods.ModConfiguration
	var err error
	if h.placement != nil {
		value, err = h.placement.Configuration(c.Request.Context(), c.Param("roomId"), c.Param("worldId"), c.Param("modId"))
	} else {
		value, err = h.mods.Configuration(c.Request.Context(), c.Param("roomId"), c.Param("worldId"), c.Param("modId"))
	}
	if err != nil {
		modFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ModHandler) previewConfiguration(c *gin.Context) {
	var request mods.ConfigUpdateRequest
	if !bindModJSON(c, &request) {
		return
	}
	var value mods.ConfigPreview
	var err error
	if h.placement != nil {
		value, err = h.placement.PreviewConfiguration(c.Request.Context(), c.Param("roomId"), c.Param("worldId"), c.Param("modId"), request)
	} else {
		value, err = h.mods.PreviewConfiguration(c.Request.Context(), c.Param("roomId"), c.Param("worldId"), c.Param("modId"), request)
	}
	if err != nil {
		modFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ModHandler) applyConfiguration(c *gin.Context) {
	if h.requirePublication(c) {
		return
	}
	var request mods.ConfigUpdateRequest
	if !bindModJSON(c, &request) {
		return
	}
	roomID, worldID, modID := c.Param("roomId"), c.Param("worldId"), c.Param("modId")
	var previewErr error
	if h.placement != nil {
		_, previewErr = h.placement.PreviewConfiguration(c.Request.Context(), roomID, worldID, modID, request)
	} else {
		_, previewErr = h.mods.PreviewConfiguration(c.Request.Context(), roomID, worldID, modID, request)
	}
	if previewErr != nil {
		modFailure(c, previewErr)
		return
	}
	h.submit(c, "mod.configuration.apply", roomID, worldID, modID, "Workshop "+modID, func(ctx context.Context, jobID string) (mods.ActionResult, error) {
		result, err := h.mods.ApplyConfiguration(ctx, jobID, roomID, worldID, modID, request)
		return mods.ActionResult{ModIDs: []string{modID}, ProtectionBackupID: result.ProtectionBackupID, Message: fmt.Sprintf("Mod 配置已应用（%d 项变更）", len(result.Changes))}, err
	})
}

func (h *ModHandler) requirePublication(c *gin.Context) bool {
	if h.placement == nil {
		return false
	}
	Failure(c, http.StatusConflict, "MOD_PUBLICATION_REQUIRED", "房间 Mod 修改必须先预览并通过 Placement 发布", nil)
	return true
}

type modAction func(context.Context, string) (mods.ActionResult, error)

func (h *ModHandler) submit(c *gin.Context, kind, roomID, worldID, targetID, targetName string, action modAction) {
	targets := []jobs.TargetSpec{{ID: targetID, Name: targetName}}
	job, err := h.jobs.SubmitFactory(kind, roomID, worldID, targets, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			result, actionErr := action(ctx, job.ID)
			if actionErr != nil {
				report(jobs.TargetResult{TargetID: targetID, Status: jobs.StatusFailed, Error: modJobError(actionErr)})
				return nil
			}
			message := result.Message
			if result.ProtectionBackupID != "" {
				message += "；保护备份 ID：" + result.ProtectionBackupID
			}
			report(jobs.TargetResult{TargetID: targetID, Status: jobs.StatusSucceeded, Message: message})
			return nil
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建 Mod 任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func modJobError(err error) *jobs.Error {
	code := "MOD_OPERATION_FAILED"
	message := err.Error()
	switch {
	case errors.Is(err, context.Canceled):
		code = "JOB_CANCELED"
		message = "任务已取消"
	case errors.Is(err, mods.ErrSteamCMDUnavailable):
		code = "STEAMCMD_UNAVAILABLE"
		message = "SteamCMD 不可用，请在系统设置中检查可执行文件路径"
	case errors.Is(err, mods.ErrSteamCMDDownload):
		code = "STEAMCMD_DOWNLOAD_FAILED"
		message = "SteamCMD 下载失败，请查看服务日志中的 SteamCMD 输出"
	case errors.Is(err, mods.ErrWorkshopItemMissing):
		code = "WORKSHOP_DOWNLOAD_MISSING"
		message = "SteamCMD 已结束，但未在配置的 Workshop 内容目录中找到模组文件"
	case errors.Is(err, mods.ErrModInfoUnavailable):
		code = "MODINFO_UNAVAILABLE"
		message = "下载目录中缺少安全可读的 modinfo.lua"
	case errors.Is(err, mods.ErrRevisionConflict):
		code = "CONFIG_REVISION_CONFLICT"
	case errors.Is(err, mods.ErrConfirmationNeeded):
		code = "CONFIRMATION_REQUIRED"
	case errors.Is(err, mods.ErrModNotConfigured):
		code = "MOD_NOT_CONFIGURED"
	case errors.Is(err, mods.ErrModNotDownloaded):
		code = "MOD_NOT_DOWNLOADED"
		message = "请先将 Mod 下载到当前运行节点"
	case errors.Is(err, mods.ErrNoChanges):
		code = "NO_CHANGES"
	case errors.Is(err, runtimeguard.ErrRemoteMutationUnavailable):
		code = runtimeguard.ErrorCode
	}
	return &jobs.Error{Code: code, Message: message}
}

func modPagination(c *gin.Context) (int, int, bool) {
	page, pageSize := 1, 20
	var err error
	if value := c.Query("page"); value != "" {
		page, err = strconv.Atoi(value)
		if err != nil {
			Failure(c, http.StatusUnprocessableEntity, "INVALID_MOD_SEARCH", "搜索分页参数无效", gin.H{"fields": gin.H{"page": "必须为整数"}})
			return 0, 0, false
		}
	}
	if value := c.Query("pageSize"); value != "" {
		pageSize, err = strconv.Atoi(value)
		if err != nil {
			Failure(c, http.StatusUnprocessableEntity, "INVALID_MOD_SEARCH", "搜索分页参数无效", gin.H{"fields": gin.H{"pageSize": "必须为整数"}})
			return 0, 0, false
		}
	}
	return page, pageSize, true
}

func modSearchOptions(c *gin.Context) (mods.SearchOptions, bool) {
	page, pageSize, ok := modPagination(c)
	if !ok {
		return mods.SearchOptions{}, false
	}
	days := 7
	if value := c.Query("days"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			Failure(c, http.StatusUnprocessableEntity, "INVALID_MOD_SEARCH", "搜索时间参数无效", gin.H{"fields": gin.H{"days": "必须为整数"}})
			return mods.SearchOptions{}, false
		}
		days = parsed
	}
	tags := c.QueryArray("tags")
	if value := c.Query("tag"); value != "" {
		tags = append(tags, value)
	}
	if len(tags) == 1 && strings.Contains(tags[0], ",") {
		tags = strings.Split(tags[0], ",")
	}
	return mods.SearchOptions{
		Query: c.Query("query"), Sort: mods.SearchSort(c.Query("sort")), Days: days,
		Tags: tags, Page: page, PageSize: pageSize,
	}, true
}

func bindModJSON(c *gin.Context, target interface{}) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "请求内容不是有效的 Mod 配置", nil)
		return false
	}
	return true
}

func modFailure(c *gin.Context, err error) {
	var fieldError *mods.FieldError
	var conflict *mods.RevisionConflictError
	switch {
	case errors.As(err, &fieldError):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_MOD_CONFIGURATION", "Mod 字段无效", gin.H{"fields": fieldError.Fields})
	case errors.As(err, &conflict):
		Failure(c, http.StatusConflict, "CONFIG_REVISION_CONFLICT", "Mod 配置已被其他操作修改，请刷新后重试", gin.H{"currentRevision": conflict.CurrentRevision})
	case errors.Is(err, mods.ErrInvalidModID):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_MOD_ID", "Workshop ID 必须为数字", nil)
	case errors.Is(err, mods.ErrNoChanges):
		Failure(c, http.StatusConflict, "NO_MOD_CHANGES", "Mod 配置没有变化", nil)
	case errors.Is(err, mods.ErrConfirmationNeeded):
		Failure(c, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "请输入完整房间名称确认此操作", nil)
	case errors.Is(err, mods.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_NOT_MANAGED", "接管房间后才能管理 Mod", nil)
	case errors.Is(err, runtimeguard.ErrRemoteMutationUnavailable):
		Failure(c, http.StatusConflict, runtimeguard.ErrorCode, "房间包含远程分片；分布式 Mod 发布尚未开放，已阻止修改控制端本机文件", nil)
	case errors.Is(err, mods.ErrModNotConfigured):
		Failure(c, http.StatusNotFound, "MOD_NOT_CONFIGURED", "所选世界未配置该 Mod", nil)
	case errors.Is(err, mods.ErrModNotDownloaded):
		Failure(c, http.StatusConflict, "MOD_NOT_DOWNLOADED", "请先将 Mod 下载到当前运行节点", nil)
	case errors.Is(err, mods.ErrModInfoUnavailable):
		Failure(c, http.StatusConflict, "MODINFO_UNAVAILABLE", "下载并校验 Mod 后才能编辑配置", nil)
	case errors.Is(err, mods.ErrSteamKeyRequired):
		Failure(c, http.StatusConflict, "STEAM_API_KEY_REQUIRED", "按名称搜索需要配置 Steam Web API Key；仍可直接输入 Workshop ID", nil)
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "房间或世界不存在", nil)
	case errors.Is(err, rooms.ErrInvalidID), errors.Is(err, rooms.ErrUnsafePath):
		Failure(c, http.StatusBadRequest, "INVALID_MOD_RESOURCE", "Mod 资源路径无效", nil)
	default:
		Failure(c, http.StatusInternalServerError, "MOD_OPERATION_FAILED", "Mod 操作失败", nil)
	}
}
