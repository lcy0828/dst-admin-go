package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"dont/internal/mods"
	"dont/internal/systemsettings"
	"github.com/gin-gonic/gin"
)

type WorkshopHandler struct {
	workshop *mods.Workshop
	settings *systemsettings.Service
}

func NewWorkshopHandler(workshop *mods.Workshop, settings *systemsettings.Service) *WorkshopHandler {
	return &WorkshopHandler{workshop: workshop, settings: settings}
}
func (h *WorkshopHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/system/workshop", h.get)
	v2.PUT("/system/workshop", h.save)
	v2.POST("/system/workshop/actions/test", h.test)
	v2.POST("/mods/metadata/enrichment", h.enrich)
	v2.GET("/mods/:modId/thumbnail", h.image)
}
func (h *WorkshopHandler) get(c *gin.Context) {
	settings, err := h.settings.Settings()
	if err != nil {
		systemSettingsFailure(c, err)
		return
	}
	fields := []systemsettings.Field{}
	for _, field := range settings.Fields {
		if field.Group == "workshop" || field.ID == "mod.steamAPIKey" {
			fields = append(fields, field)
		}
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, gin.H{"revision": settings.Revision, "fields": fields, "configVersion": h.workshop.Version()})
}
func (h *WorkshopHandler) save(c *gin.Context) {
	var input systemsettings.Input
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	if c.ShouldBindJSON(&input) != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "工坊设置参数无效", nil)
		return
	}
	if _, err := h.settings.ApplyWorkshop(c.Request.Context(), input); err != nil {
		systemSettingsFailure(c, err)
		return
	}
	h.get(c)
}
func (h *WorkshopHandler) test(c *gin.Context) {
	var input struct {
		systemsettings.Input
		SampleID string `json:"sampleId,omitempty"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	if c.ShouldBindJSON(&input) != nil || (input.SampleID != "" && !mods.ValidID(input.SampleID)) {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "工坊检测参数无效", nil)
		return
	}
	config, err := h.settings.ResolveWorkshopTest(input.Input)
	if err != nil {
		systemSettingsFailure(c, err)
		return
	}
	result, err := h.workshop.Test(c.Request.Context(), config, input.SampleID)
	if err != nil {
		modFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, result)
}
func (h *WorkshopHandler) enrich(c *gin.Context) {
	var input struct {
		IDs           []string `json:"ids"`
		ConfigVersion string   `json:"configVersion,omitempty"`
		Community     bool     `json:"community,omitempty"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	if c.ShouldBindJSON(&input) != nil || len(input.IDs) == 0 {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "模组资料参数无效", nil)
		return
	}
	version := h.workshop.Version()
	if input.ConfigVersion != "" && input.ConfigVersion != version {
		modFailure(c, &mods.NetworkError{Code: "WORKSHOP_CONFIG_CHANGED"})
		return
	}
	var items map[string]mods.SteamMod
	var err error
	if input.Community {
		items, err = h.workshop.Enrich(c.Request.Context(), input.IDs)
	} else {
		items, err = h.workshop.Authors(c.Request.Context(), input.IDs)
	}
	if h.workshop.Version() != version {
		modFailure(c, &mods.NetworkError{Code: "WORKSHOP_CONFIG_CHANGED"})
		return
	}
	if err != nil && len(items) == 0 {
		modFailure(c, err)
		return
	}
	warning := ""
	if err != nil {
		warning = err.Error()
	}
	Success(c, http.StatusOK, gin.H{"items": items, "warning": warning, "configVersion": version})
}
func (h *WorkshopHandler) image(c *gin.Context) {
	size := 184
	if value := c.Query("size"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			modFailure(c, mods.ErrInvalidRequest)
			return
		}
		size = parsed
	}
	value, err := h.workshop.Thumbnail(c.Request.Context(), c.Param("modId"), size)
	if err != nil {
		c.Header("Cache-Control", "no-store")
		modFailure(c, err)
		return
	}
	c.Header("Cache-Control", "private, max-age=3600")
	c.Header("ETag", value.ETag)
	c.Header("X-Content-Type-Options", "nosniff")
	for _, candidate := range strings.Split(c.GetHeader("If-None-Match"), ",") {
		if strings.TrimSpace(candidate) == value.ETag || strings.TrimSpace(candidate) == "*" {
			c.Status(http.StatusNotModified)
			return
		}
	}
	c.Data(http.StatusOK, value.ContentType, value.Data)
}
