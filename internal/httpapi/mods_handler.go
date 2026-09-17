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
	"time"

	"dont/internal/jobs"
	"dont/internal/mods"
	"dont/internal/operationprogress"
	"dont/internal/requesttiming"
	"dont/internal/rooms"
	"dont/internal/runtimedriver"
	"dont/internal/runtimeguard"

	"github.com/gin-gonic/gin"
)

type ModHandler struct {
	mods                  modService
	placement             modPlacementReader
	installationUpdater   modInstallationUpdater
	installationRefresher modInstallationUpdateRefresher
	roomInstaller         modRoomInstaller
	jobs                  *jobs.Service
}

type modPlacementReader interface {
	RoomList(context.Context, string) (mods.ModList, error)
	ConfigurationFile(context.Context, string, string) (mods.ConfigurationFile, error)
	Configuration(context.Context, string, string, string) (mods.ModConfiguration, error)
	PreviewConfiguration(context.Context, string, string, string, mods.ConfigUpdateRequest) (mods.ConfigPreview, error)
}

type modConfigurationApplier interface {
	ApplyConfiguration(context.Context, string, string, string, mods.ConfigUpdateRequest) (mods.ConfigApplyResult, error)
}

type modEnabledSetter interface {
	SetEnabled(context.Context, string, string, mods.EnableRequest) (mods.ConfigApplyResult, error)
}

type modProfileReader interface {
	RoomProfile(context.Context, string) (mods.RoomModProfile, error)
}

type modInstallationInventoryReader interface {
	InstallationModInventory(context.Context, string, string) (mods.RuntimeInstallationModInventory, error)
}

type modInstallationUpdater interface {
	UpdateInstallationMods(context.Context, string, string, []string, io.Writer) (mods.ActionResult, error)
}

type modInstallationUpdateRefresher interface {
	RefreshInstallation(context.Context, string, string) error
}

type modRoomInstaller interface {
	Install(context.Context, string, string, mods.InstallRequest, io.Writer) (mods.ActionResult, error)
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

func (h *ModHandler) ConfigureInstallationUpdater(updater modInstallationUpdater) {
	h.installationUpdater = updater
}

func (h *ModHandler) ConfigureInstallationUpdateRefresher(refresher modInstallationUpdateRefresher) {
	h.installationRefresher = refresher
}

func (h *ModHandler) ConfigureRoomInstaller(installer modRoomInstaller) {
	h.roomInstaller = installer
}

func (h *ModHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/mods/search", h.search)
	v2.GET("/mods/metadata", h.metadata)
	v2.GET("/mods/library", h.library)
	v2.POST("/mods/library/actions/download", h.download)
	v2.POST("/mods/library/:modId/actions/update", h.updateLibrary)
	v2.GET("/mods/:modId", h.details)
	v2.GET("/runtime-targets/:targetId/installations/:installationId/mods", h.installationInventory)
	v2.POST("/runtime-targets/:targetId/installations/:installationId/mods/:modId/actions/download", h.downloadInstallationMod)
	v2.POST("/runtime-targets/:targetId/installations/:installationId/mods/:modId/actions/update", h.updateInstallationMod)
	v2.POST("/runtime-targets/:targetId/installations/:installationId/mods/actions/update-outdated", h.updateOutdatedInstallationMods)

	room := v2.Group("/rooms/:roomId")
	room.GET("/mods", h.list)
	room.GET("/mod-profile", h.roomProfile)
	room.PUT("/mods/:modId/configuration-mode", h.setConfigurationMode)
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

func (h *ModHandler) setConfigurationMode(c *gin.Context) {
	service, ok := h.placement.(interface {
		SetConfigurationMode(context.Context, string, string, string) error
	})
	if !ok {
		Failure(c, http.StatusServiceUnavailable, "MOD_CONFIGURATION_MODE_UNAVAILABLE", "Mod 配置模式保存不可用", nil)
		return
	}
	var input struct {
		Mode string `json:"mode"`
	}
	if !bindModJSON(c, &input) {
		return
	}
	if err := service.SetConfigurationMode(c.Request.Context(), c.Param("roomId"), c.Param("modId"), input.Mode); err != nil {
		modFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"mode": input.Mode})
}

func (h *ModHandler) installationInventory(c *gin.Context) {
	reader, ok := h.placement.(modInstallationInventoryReader)
	if !ok {
		Failure(c, http.StatusServiceUnavailable, "MOD_RUNTIME_INVENTORY_UNAVAILABLE", "机器模组内容读取尚不可用", nil)
		return
	}
	var value mods.RuntimeInstallationModInventory
	var err error
	if c.Query("view") == "facts" {
		facts, supported := h.placement.(interface {
			InstallationModFacts(context.Context, string, string) (mods.RuntimeInstallationModInventory, error)
		})
		if !supported {
			Failure(c, http.StatusServiceUnavailable, "MOD_RUNTIME_INVENTORY_UNAVAILABLE", "机器模组内容读取尚不可用", nil)
			return
		}
		value, err = facts.InstallationModFacts(c.Request.Context(), c.Param("targetId"), c.Param("installationId"))
	} else {
		value, err = reader.InstallationModInventory(c.Request.Context(), c.Param("targetId"), c.Param("installationId"))
	}
	if err != nil {
		Failure(c, http.StatusServiceUnavailable, "MOD_RUNTIME_INVENTORY_FAILED", "机器模组内容读取失败", gin.H{"reason": err.Error()})
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ModHandler) updateInstallationMod(c *gin.Context) {
	h.submitInstallationMod(c, "mod.installation.update", true)
}

func (h *ModHandler) downloadInstallationMod(c *gin.Context) {
	h.submitInstallationMod(c, "mod.installation.download", false)
}

func (h *ModHandler) submitInstallationMod(c *gin.Context, kind string, refreshUpdateState bool) {
	modID := c.Param("modId")
	if !mods.ValidID(modID) {
		modFailure(c, mods.ErrInvalidModID)
		return
	}
	if h.installationUpdater == nil || h.jobs == nil {
		Failure(c, http.StatusServiceUnavailable, "MOD_INSTALLATION_UPDATE_UNAVAILABLE", "机器模组更新尚不可用", nil)
		return
	}
	targetID, installationID := c.Param("targetId"), c.Param("installationId")
	h.submit(c, kind, "", "", modID, "Workshop "+modID, func(ctx context.Context, _ string) (mods.ActionResult, error) {
		result, err := h.installationUpdater.UpdateInstallationMods(ctx, targetID, installationID, []string{modID}, log.Writer())
		if err != nil || !refreshUpdateState {
			return result, err
		}
		return h.refreshInstallationUpdateState(ctx, targetID, installationID, result)
	})
}

func (h *ModHandler) updateOutdatedInstallationMods(c *gin.Context) {
	reader, readable := h.placement.(modInstallationInventoryReader)
	if !readable || h.installationUpdater == nil || h.jobs == nil {
		Failure(c, http.StatusServiceUnavailable, "MOD_INSTALLATION_UPDATE_UNAVAILABLE", "机器模组更新尚不可用", nil)
		return
	}
	targetID, installationID := c.Param("targetId"), c.Param("installationId")
	inventory, err := reader.InstallationModInventory(c.Request.Context(), targetID, installationID)
	if err != nil {
		Failure(c, http.StatusServiceUnavailable, "MOD_RUNTIME_INVENTORY_FAILED", "机器模组内容读取失败", gin.H{"reason": err.Error()})
		return
	}
	modIDs := make([]string, 0, inventory.Outdated)
	for _, item := range inventory.Items {
		if item.VersionStatus == "outdated" {
			modIDs = append(modIDs, item.ID)
		}
	}
	targetName := fmt.Sprintf("%s / %s", targetID, installationID)
	h.submit(c, "mod.installation.update-all", "", "", targetID+":"+installationID, targetName, func(ctx context.Context, _ string) (mods.ActionResult, error) {
		result := mods.ActionResult{Message: "当前机器的模组已是最新版本"}
		if len(modIDs) == 0 {
			return h.refreshInstallationUpdateState(ctx, targetID, installationID, result)
		}
		return h.updateInstallationContent(ctx, targetID, installationID, modIDs)
	})
}

func (h *ModHandler) updateInstallationContent(ctx context.Context, targetID, installationID string, modIDs []string) (mods.ActionResult, error) {
	result, err := h.installationUpdater.UpdateInstallationMods(ctx, targetID, installationID, modIDs, log.Writer())
	if err != nil {
		return result, err
	}
	return h.refreshInstallationUpdateState(ctx, targetID, installationID, result)
}

func (h *ModHandler) refreshInstallationUpdateState(ctx context.Context, targetID, installationID string, result mods.ActionResult) (mods.ActionResult, error) {
	if h.installationRefresher == nil {
		return result, nil
	}
	if err := h.installationRefresher.RefreshInstallation(ctx, targetID, installationID); err != nil {
		message := strings.TrimSpace(result.Message)
		if message == "" {
			message = "机器模组内容已更新"
		}
		result.Message = message
		result.Warnings = append(result.Warnings, fmt.Sprintf("关联房间的更新检查未启动: %v", err))
		log.Printf("[ModUpdates] target_id=%s installation_id=%s content_updated=true check_error=%v", targetID, installationID, err)
	}
	return result, nil
}

func (h *ModHandler) roomProfile(c *gin.Context) {
	reader, ok := h.placement.(modProfileReader)
	if !ok {
		Failure(c, http.StatusServiceUnavailable, "MOD_PROFILE_UNAVAILABLE", "房间模组配置视图尚不可用", nil)
		return
	}
	value, err := reader.RoomProfile(c.Request.Context(), c.Param("roomId"))
	if err != nil {
		modFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
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
		if h.roomInstaller != nil {
			return h.roomInstaller.Install(ctx, jobID, roomID, mods.InstallRequest{
				ModID: modID, WorldIDs: request.WorldIDs, Enabled: request.Enabled, IncludeDependencies: request.IncludeDependencies,
			}, log.Writer())
		}
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
	item := value.Items[0]
	item.MetadataWarning = value.Warning
	Success(c, http.StatusOK, item)
}

func (h *ModHandler) metadata(c *gin.Context) {
	ids := strings.Split(c.Query("ids"), ",")
	if len(ids) > 100 {
		modFailure(c, mods.ErrInvalidRequest)
		return
	}
	for _, id := range ids {
		if !mods.ValidID(id) {
			modFailure(c, mods.ErrInvalidModID)
			return
		}
	}
	if c.Query("cached") == "true" {
		reader, ok := h.mods.(interface {
			CachedMetadata(context.Context, []string) (map[string]mods.SteamMod, error)
		})
		if !ok {
			Success(c, http.StatusOK, gin.H{"items": map[string]mods.SteamMod{}})
			return
		}
		items, err := reader.CachedMetadata(c.Request.Context(), ids)
		if err != nil {
			modFailure(c, err)
			return
		}
		Success(c, http.StatusOK, gin.H{"items": items})
		return
	}
	reader, ok := h.mods.(interface {
		Metadata(context.Context, []string) (map[string]mods.SteamMod, error)
	})
	if !ok {
		Failure(c, http.StatusServiceUnavailable, "MOD_METADATA_UNAVAILABLE", "Steam 模组信息暂不可用", nil)
		return
	}
	ctx, timing := requesttiming.New(c.Request.Context())
	items, err := reader.Metadata(ctx, ids)
	finishReadTiming(c, timing, err)
	warning := ""
	if err != nil {
		warning = err.Error()
	}
	Success(c, http.StatusOK, gin.H{"items": items, "warning": warning})
}

func (h *ModHandler) list(c *gin.Context) {
	ctx, timing := requesttiming.New(c.Request.Context())
	var value mods.ModList
	var err error
	if c.Query("view") == "facts" {
		reader, ok := h.placement.(interface {
			RoomFacts(context.Context, string) (mods.ModList, error)
		})
		if !ok {
			Failure(c, http.StatusServiceUnavailable, "MOD_RUNTIME_INVENTORY_UNAVAILABLE", "房间模组状态读取尚不可用", nil)
			return
		}
		value, err = reader.RoomFacts(ctx, c.Param("roomId"))
	} else if h.placement != nil {
		value, err = h.placement.RoomList(ctx, c.Param("roomId"))
	} else {
		value, err = h.mods.List(ctx, c.Param("roomId"))
	}
	finishReadTiming(c, timing, err)
	if err != nil {
		modFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *ModHandler) install(c *gin.Context) {
	if h.roomInstaller == nil && h.requirePublication(c) {
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
		if h.roomInstaller != nil {
			return h.roomInstaller.Install(ctx, jobID, roomID, request, log.Writer())
		}
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
	var request mods.EnableRequest
	if !bindModJSON(c, &request) {
		return
	}
	roomID, modID := c.Param("roomId"), c.Param("modId")
	if !mods.ValidID(modID) {
		modFailure(c, mods.ErrInvalidModID)
		return
	}
	if setter, ok := h.placement.(modEnabledSetter); ok {
		result, err := setter.SetEnabled(c.Request.Context(), roomID, modID, request)
		if err != nil {
			modConfigurationFailure(c, result, err)
			return
		}
		Success(c, http.StatusOK, result)
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
	var request mods.ConfigUpdateRequest
	if !bindModJSON(c, &request) {
		return
	}
	roomID, worldID, modID := c.Param("roomId"), c.Param("worldId"), c.Param("modId")
	if h.placement != nil {
		applier, ok := h.placement.(modConfigurationApplier)
		if !ok {
			Failure(c, http.StatusServiceUnavailable, "MOD_CONFIGURATION_APPLY_UNAVAILABLE", "Mod 配置写入尚不可用", nil)
			return
		}
		result, err := applier.ApplyConfiguration(c.Request.Context(), roomID, worldID, modID, request)
		if err != nil {
			modConfigurationFailure(c, result, err)
			return
		}
		Success(c, http.StatusOK, result)
		return
	}
	result, err := h.mods.ApplyConfiguration(c.Request.Context(), "", roomID, worldID, modID, request)
	if err != nil {
		modFailure(c, err)
		return
	}
	Success(c, http.StatusOK, result)
}

func modConfigurationFailure(c *gin.Context, result mods.ConfigApplyResult, err error) {
	if result.PublishedTargets == 0 {
		modFailure(c, err)
		return
	}
	Failure(c, http.StatusConflict, "MOD_CONFIGURATION_PARTIALLY_APPLIED",
		fmt.Sprintf("已保存 %d 个世界；其余世界保存失败，请刷新配置后重试: %v", result.PublishedTargets, err),
		gin.H{"publishedWorlds": result.PublishedTargets})
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
	requestID := RequestID(c)
	job, err := h.jobs.SubmitFactory(kind, roomID, worldID, targets, func(job jobs.Job) jobs.Runner {
		return func(ctx context.Context, report func(jobs.TargetResult)) error {
			started := time.Now()
			ctx = attachModJobProgress(ctx, h.jobs, job.ID)
			result, actionErr := action(ctx, job.ID)
			if actionErr != nil {
				log.Printf("[ModJob] request_id=%s job_id=%s kind=%s room_id=%s status=failed duration_ms=%d error=%v", requestID, job.ID, kind, roomID, time.Since(started).Milliseconds(), actionErr)
				report(jobs.TargetResult{TargetID: targetID, Status: jobs.StatusFailed, Error: modJobError(actionErr)})
				return nil
			}
			log.Printf("[ModJob] request_id=%s job_id=%s kind=%s room_id=%s status=succeeded duration_ms=%d", requestID, job.ID, kind, roomID, time.Since(started).Milliseconds())
			message := result.Message
			if result.ProtectionBackupID != "" {
				message += "；保护备份 ID：" + result.ProtectionBackupID
			}
			var warning *jobs.Error
			if len(result.Warnings) > 0 {
				warning = &jobs.Error{Code: "MOD_ACTION_WARNING", Message: strings.Join(result.Warnings, "; ")}
			}
			report(jobs.TargetResult{TargetID: targetID, Status: jobs.StatusSucceeded, Message: message, Warning: warning})
			return nil
		}
	})
	if err != nil {
		Failure(c, http.StatusInternalServerError, "JOB_CREATE_FAILED", "无法创建 Mod 任务", nil)
		return
	}
	Success(c, http.StatusAccepted, job)
}

func attachModJobProgress(ctx context.Context, service *jobs.Service, jobID string) context.Context {
	return operationprogress.WithReporter(ctx, func(update operationprogress.Update) {
		progress := modJobProgress(update)
		message := strings.TrimSpace(update.Message)
		if service == nil || progress <= 0 || progress >= 100 || message == "" {
			return
		}
		_, _ = service.UpdateProgressDetail(jobID, jobs.ProgressUpdate{
			Progress: progress, Message: message,
			Detail:       &jobs.ProgressDetail{Stage: update.Stage, WorkshopID: update.WorkshopID, CurrentItem: update.CurrentItem, TotalItems: update.TotalItems, TargetID: update.TargetID, InstallationID: update.InstallationID, Items: update.Items},
			CurrentBytes: update.CurrentBytes, TotalBytes: update.TotalBytes, BytesPerSecond: update.BytesPerSecond,
		})
	})
}

func modJobProgress(update operationprogress.Update) int {
	percent := update.Percent
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	switch update.Stage {
	case operationprogress.StageStartConfiguration:
		return 2
	case operationprogress.StageStartRouting:
		return 4
	case operationprogress.StageStartMods:
		return 5
	case operationprogress.StageModInspect:
		return 5 + percent*5/100
	case operationprogress.StageModCache:
		return 10 + percent*55/100
	case operationprogress.StageModPrepare:
		return 65 + percent*15/100
	case operationprogress.StageModPublish:
		return 80 + percent*15/100
	case operationprogress.StageModComplete:
		return 95 + percent*4/100
	case operationprogress.StageModDone:
		return 99
	default:
		if percent >= 100 {
			return 99
		}
		return percent
	}
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
		message = err.Error()
	case errors.Is(err, mods.ErrWorkshopItemMissing):
		code = "WORKSHOP_DOWNLOAD_MISSING"
		message = "SteamCMD 已结束，但未在配置的 Workshop 内容目录中找到模组文件"
	case errors.Is(err, mods.ErrModInfoUnavailable):
		code = "MODINFO_UNAVAILABLE"
		message = "下载目录中缺少安全可读的 modinfo.lua"
	case errors.Is(err, mods.ErrRevisionConflict):
		code = "CONFIG_REVISION_CONFLICT"
	case errors.Is(err, runtimedriver.ErrTopologyChanged):
		code = "TOPOLOGY_CHANGED"
		message = "Mod 配置的运行位置已改变，请刷新后重试"
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
	case errors.Is(err, runtimedriver.ErrTopologyChanged):
		Failure(c, http.StatusConflict, "TOPOLOGY_CHANGED", "Mod 配置的运行位置已改变，请刷新后重试", nil)
	case errors.Is(err, mods.ErrInvalidModID):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_MOD_ID", "Workshop ID 必须为数字", nil)
	case errors.Is(err, mods.ErrInvalidRequest):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_MOD_REQUEST", "Mod 请求参数无效", nil)
	case errors.Is(err, mods.ErrNoChanges):
		Failure(c, http.StatusConflict, "NO_MOD_CHANGES", "Mod 配置没有变化", nil)
	case errors.Is(err, mods.ErrConfirmationNeeded):
		Failure(c, http.StatusUnprocessableEntity, "CONFIRMATION_REQUIRED", "请输入完整房间名称确认此操作", nil)
	case errors.Is(err, mods.ErrRoomNotManaged):
		Failure(c, http.StatusConflict, "ROOM_UNAVAILABLE", "房间当前不可用，请检查运行节点与拓扑状态", nil)
	case errors.Is(err, runtimeguard.ErrRemoteMutationUnavailable):
		Failure(c, http.StatusConflict, runtimeguard.ErrorCode, "房间包含远程分片；分布式 Mod 发布尚未开放，已阻止修改控制端本机文件", nil)
	case errors.Is(err, mods.ErrModNotConfigured):
		Failure(c, http.StatusNotFound, "MOD_NOT_CONFIGURED", "所选世界未配置该 Mod", nil)
	case errors.Is(err, mods.ErrModNotDownloaded):
		Failure(c, http.StatusConflict, "MOD_NOT_DOWNLOADED", "请先将 Mod 下载到当前运行节点", nil)
	case errors.Is(err, mods.ErrModInfoUnavailable):
		Failure(c, http.StatusConflict, "MODINFO_UNAVAILABLE", "运行节点上缺少该 Mod 的 modinfo.lua，请检查该节点的模组文件", nil)
	case errors.Is(err, mods.ErrSteamKeyRequired):
		Failure(c, http.StatusConflict, "STEAM_API_KEY_REQUIRED", "按名称搜索需要配置 Steam Web API Key；仍可直接输入 Workshop ID", nil)
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, rooms.ErrWorldNotFound):
		Failure(c, http.StatusNotFound, "RESOURCE_NOT_FOUND", "房间或世界不存在", nil)
	case errors.Is(err, rooms.ErrInvalidID), errors.Is(err, rooms.ErrUnsafePath):
		Failure(c, http.StatusBadRequest, "INVALID_MOD_RESOURCE", "Mod 资源路径无效", nil)
	default:
		log.Printf("[ModsHTTP] request_id=%s method=%s path=%s error=%v", RequestID(c), c.Request.Method, c.Request.URL.Path, err)
		Failure(c, http.StatusInternalServerError, "MOD_OPERATION_FAILED", "Mod 操作失败", gin.H{"reason": err.Error()})
	}
}
