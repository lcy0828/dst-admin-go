package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"dont/internal/announcements"

	"github.com/gin-gonic/gin"
)

type AnnouncementService interface {
	List() ([]announcements.Announcement, error)
	Get(uint64) (announcements.Announcement, error)
	Create(announcements.Input) (announcements.Announcement, error)
	Update(uint64, announcements.Input) (announcements.Announcement, error)
	Delete(uint64) error
}

type AnnouncementHandler struct {
	service AnnouncementService
}

func NewAnnouncementHandler(service AnnouncementService) *AnnouncementHandler {
	return &AnnouncementHandler{service: service}
}

func (h *AnnouncementHandler) Register(v2 *gin.RouterGroup) {
	v2.GET("/announcements", h.list)
	v2.POST("/announcements", h.create)
	v2.GET("/announcements/:announcementId", h.get)
	v2.PUT("/announcements/:announcementId", h.update)
	v2.DELETE("/announcements/:announcementId", h.delete)
}

func (h *AnnouncementHandler) list(c *gin.Context) {
	items, err := h.service.List()
	if err != nil {
		announcementFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, items)
}

func (h *AnnouncementHandler) get(c *gin.Context) {
	id, ok := announcementID(c)
	if !ok {
		return
	}
	value, err := h.service.Get(id)
	if err != nil {
		announcementFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, value)
}

func (h *AnnouncementHandler) create(c *gin.Context) {
	var input announcements.Input
	if err := c.ShouldBindJSON(&input); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "公告请求不是有效 JSON", nil)
		return
	}
	value, err := h.service.Create(input)
	if err != nil {
		announcementFailure(c, err)
		return
	}
	Success(c, http.StatusCreated, value)
}

func (h *AnnouncementHandler) update(c *gin.Context) {
	id, ok := announcementID(c)
	if !ok {
		return
	}
	var input announcements.Input
	if err := c.ShouldBindJSON(&input); err != nil {
		Failure(c, http.StatusBadRequest, "INVALID_JSON", "公告请求不是有效 JSON", nil)
		return
	}
	value, err := h.service.Update(id, input)
	if err != nil {
		announcementFailure(c, err)
		return
	}
	Success(c, http.StatusOK, value)
}

func (h *AnnouncementHandler) delete(c *gin.Context) {
	id, ok := announcementID(c)
	if !ok {
		return
	}
	if err := h.service.Delete(id); err != nil {
		announcementFailure(c, err)
		return
	}
	Success(c, http.StatusOK, gin.H{"id": id, "deleted": true})
}

func announcementID(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("announcementId"), 10, 64)
	if err != nil || id == 0 {
		Failure(c, http.StatusBadRequest, "INVALID_ANNOUNCEMENT_ID", "公告 ID 无效", nil)
		return 0, false
	}
	return id, true
}

func announcementFailure(c *gin.Context, err error) {
	var fieldErr *announcements.FieldError
	switch {
	case errors.As(err, &fieldErr):
		Failure(c, http.StatusUnprocessableEntity, "INVALID_ANNOUNCEMENT", "公告内容校验失败", fieldErr.Fields)
	case errors.Is(err, announcements.ErrNotFound):
		Failure(c, http.StatusNotFound, "ANNOUNCEMENT_NOT_FOUND", "公告不存在", nil)
	default:
		Failure(c, http.StatusInternalServerError, "ANNOUNCEMENT_OPERATION_FAILED", "公告操作失败", nil)
	}
}
