package httpapi

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"dont/internal/agents"
	"dont/internal/luajit"
	"github.com/gin-gonic/gin"
)

type LuaJITHandler struct {
	service *luajit.Service
	store   *luajit.Store
}

func NewLuaJITHandler(service *luajit.Service, store *luajit.Store) *LuaJITHandler {
	return &LuaJITHandler{service: service, store: store}
}
func (h *LuaJITHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/runtime-targets/luajit", h.catalog)
	v2.GET("/runtime-targets/luajit/status", h.inspect)
	v2.POST("/runtime-targets/luajit/packages", h.upload)
	v2.POST("/runtime-targets/luajit/actions/download", h.download)
	v2.POST("/runtime-targets/luajit/actions/install", h.install)
}
func (h *LuaJITHandler) RegisterDownloads(router *gin.Engine) {
	router.GET("/luajit-packages/:releaseId", func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		f, err := h.store.Open(c.Param("releaseId"), strings.TrimPrefix(header, "Bearer "))
		if err != nil {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Header("Content-Type", "application/zip")
		http.ServeContent(c.Writer, c.Request, "luajit.zip", info.ModTime(), f)
	})
}
func (h *LuaJITHandler) catalog(c *gin.Context) {
	value, err := h.service.Catalog(c.Query("targetId"))
	if err != nil {
		luaJITFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, value)
}
func (h *LuaJITHandler) inspect(c *gin.Context) {
	value, err := h.service.Inspect(c.Request.Context(), c.Query("targetId"), c.Query("installationId"), c.Query("refreshUpstream") == "true")
	if err != nil {
		luaJITFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}
func (h *LuaJITHandler) upload(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, luajit.MaxPackageBytes+(1<<20))
	if err := c.Request.ParseMultipartForm(8 << 20); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_LUAJIT_UPLOAD", "请选择不超过 256 MiB 的 ZIP 安装包", nil)
		return
	}
	defer c.Request.MultipartForm.RemoveAll()
	f, _, err := c.Request.FormFile("file")
	if err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_LUAJIT_UPLOAD", "请选择 ZIP 安装包", nil)
		return
	}
	defer f.Close()
	value, err := h.store.Save(c.Request.Context(), f, "")
	if err != nil {
		luaJITFailure(c, err)
		return
	}
	Success(c, http.StatusCreated, value)
}
func (h *LuaJITHandler) download(c *gin.Context) {
	var request struct {
		TargetID       string `json:"targetId" binding:"required"`
		InstallationID string `json:"installationId" binding:"required"`
		URL            string `json:"url" binding:"required"`
		SHA256         string `json:"sha256" binding:"required"`
	}
	if c.ShouldBindJSON(&request) != nil {
		Failure(c, http.StatusBadRequest, "INVALID_LUAJIT_SOURCE", "请选择运行机器、DST 安装并填写安装包地址和 SHA-256", nil)
		return
	}
	job, err := h.service.ImportURL(request.TargetID, request.InstallationID, request.URL, request.SHA256)
	if err != nil {
		luaJITFailure(c, err)
		return
	}
	Success(c, http.StatusAccepted, job)
}
func (h *LuaJITHandler) install(c *gin.Context) {
	var request struct {
		TargetID       string `json:"targetId" binding:"required"`
		InstallationID string `json:"installationId" binding:"required"`
		ReleaseID      string `json:"releaseId" binding:"required"`
		Source         string `json:"source"`
	}
	if c.ShouldBindJSON(&request) != nil {
		Failure(c, http.StatusBadRequest, "INVALID_LUAJIT_INSTALL", "请选择运行机器、DST 安装和 LuaJIT 版本", nil)
		return
	}
	job, err := h.service.SubmitInstall(request.TargetID, request.InstallationID, request.ReleaseID, request.Source)
	if err != nil {
		luaJITFailure(c, err)
		return
	}
	Success(c, http.StatusAccepted, job)
}
func luaJITFailure(c *gin.Context, err error) {
	status := http.StatusUnprocessableEntity
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, agents.ErrRuntimeInstallationNotRegistered) {
		status = http.StatusNotFound
	}
	if errors.Is(err, luajit.ErrRunning) || errors.Is(err, luajit.ErrBusy) {
		status = http.StatusConflict
	}
	Failure(c, status, "LUAJIT_OPERATION_FAILED", err.Error(), nil)
}
