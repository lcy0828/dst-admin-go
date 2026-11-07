package httpapi

import (
	"fmt"
	"net/http"
	"strings"

	"dont/internal/aicontrol"
	"dont/internal/apicontract"
	"github.com/gin-gonic/gin"
)

func (h *AIHandler) ConfigureResources(router http.Handler) { h.resourceRouter = router }
func (h *AIHandler) principal(c *gin.Context) aicontrol.Principal {
	if p, ok := c.Get(aiScopeKey); ok {
		if principal, ok := p.(aicontrol.Principal); ok {
			return principal
		}
	}
	if header := c.GetHeader("Authorization"); header != "" {
		scheme, secret, ok := strings.Cut(header, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			return aicontrol.Principal{}
		}
		p, err := h.service.Store.Authenticate(secret)
		if err != nil {
			return aicontrol.Principal{}
		}
		return p
	}
	return aicontrol.Principal{ID: "page"}
}
func (h *AIHandler) upload(c *gin.Context) {
	if !h.service.Enabled() {
		aiFailure(c, aicontrol.ErrUnavailable)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, aicontrol.MaxAttachmentBytes+(1<<20))
	reader, err := c.Request.MultipartReader()
	if err != nil {
		aiFailure(c, aicontrol.ErrInvalid)
		return
	}
	part, err := reader.NextPart()
	if err != nil || part.FormName() != "file" || part.FileName() == "" {
		aiFailure(c, aicontrol.ErrInvalid)
		return
	}
	defer part.Close()
	value, err := h.service.Store.SaveAttachment(c.Request.Context(), h.principal(c), part.FileName(), part, c.Request.ContentLength)
	if err != nil {
		aiFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, 201, value)
}
func (h *AIHandler) deleteAttachment(c *gin.Context) {
	if err := h.service.Store.DeleteAttachment(h.principal(c), c.Param("attachmentId")); err != nil {
		aiFailure(c, err)
		return
	}
	Success(c, 200, gin.H{"deleted": true})
}
func (h *AIHandler) privateResult(c *gin.Context) {
	if !h.service.Enabled() {
		aiFailure(c, aicontrol.ErrUnavailable)
		return
	}
	raw, err := h.service.Store.PrivateResult(h.principal(c), c.Param("resourceId"))
	if err != nil {
		aiFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Disposition", `attachment; filename="management-result.json"`)
	c.Data(200, "application/json", raw)
}
func (h *AIHandler) privateInput(c *gin.Context) {
	if !h.service.Enabled() {
		aiFailure(c, aicontrol.ErrUnavailable)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	var in struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if c.ShouldBindJSON(&in) != nil {
		aiFailure(c, aicontrol.ErrInvalid)
		return
	}
	value, err := h.service.Store.SavePrivateInput(h.principal(c), in.Name, in.Value)
	if err != nil {
		aiFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, 201, value)
}
func (h *AIHandler) resource(c *gin.Context) {
	if !h.service.Enabled() || h.resourceRouter == nil {
		aiFailure(c, aicontrol.ErrUnavailable)
		return
	}
	value, err := h.service.Store.Resource(h.principal(c), c.Param("resourceId"))
	if err != nil {
		aiFailure(c, err)
		return
	}
	catalog, err := apicontract.Default()
	if err != nil {
		aiFailure(c, err)
		return
	}
	op, ok := catalog.Operation(value.OperationID)
	if !ok || !op.Callable() || op.Method != "GET" {
		aiFailure(c, aicontrol.ErrDenied)
		return
	}
	uri, err := op.URL(value.Input)
	if err != nil {
		aiFailure(c, err)
		return
	}
	request := c.Request.Clone(c.Request.Context())
	parsed, err := http.NewRequestWithContext(request.Context(), "GET", uri, nil)
	if err != nil {
		aiFailure(c, err)
		return
	}
	request.URL = parsed.URL
	request.RequestURI = uri
	// Original authentication is rechecked by the ordinary API router. No
	// resource link grants access after logout, credential expiry or revocation.
	for key, value := range value.Input.Headers {
		request.Header.Set(key, fmt.Sprint(value))
	}
	c.Header("Cache-Control", "no-store")
	h.resourceRouter.ServeHTTP(c.Writer, request)
}
