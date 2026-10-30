package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"dont/internal/aicontrol"
	"dont/internal/authn"
	"github.com/gin-gonic/gin"
)

type AIHandler struct {
	service        *aicontrol.Service
	resourceRouter http.Handler
}

func NewAIHandler(service *aicontrol.Service) *AIHandler { return &AIHandler{service: service} }
func (h *AIHandler) Register(v2 *gin.RouterGroup) {
	h.registerBot(v2)
	g := v2.Group("/system/ai")
	g.GET("", h.status)
	g.PUT("", h.configure)
	g.POST("/actions/test", h.test)
	g.GET("/tokens", h.tokens)
	g.POST("/tokens", h.createToken)
	g.DELETE("/tokens/:tokenId", h.revoke)
	g.GET("/calls", h.history)
	g.GET("/calls/:callId", h.call)
	g.POST("/chat", h.chat)
	g.POST("/attachments", h.upload)
	g.DELETE("/attachments/:attachmentId", h.deleteAttachment)
	g.GET("/resources/:resourceId", h.resource)
	g.GET("/private-results/:resourceId", h.privateResult)
	g.POST("/private-inputs", h.privateInput)
}
func (h *AIHandler) status(c *gin.Context) {
	v, err := h.service.Store.Config()
	if err != nil {
		aiFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, 200, gin.H{"config": v, "tools": h.service.InitialTools(), "mcpPath": "/mcp"})
}
func (h *AIHandler) configure(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	var input aicontrol.ConfigInput
	if c.ShouldBindJSON(&input) != nil {
		aiFailure(c, aicontrol.ErrInvalid)
		return
	}
	v, err := h.service.Store.SaveConfig(input)
	if err != nil {
		aiFailure(c, err)
		return
	}
	Success(c, 200, v)
}
func (h *AIHandler) test(c *gin.Context) {
	release, err := h.service.Acquire()
	if err != nil {
		aiFailure(c, err)
		return
	}
	defer release()
	if err = h.service.TestProvider(c.Request.Context()); err != nil {
		aiFailure(c, err)
		return
	}
	Success(c, 200, gin.H{"connected": true})
}
func (h *AIHandler) tokens(c *gin.Context) {
	items, err := h.service.Store.Tokens()
	if err != nil {
		aiFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, 200, gin.H{"items": items})
}
func (h *AIHandler) createToken(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	var input aicontrol.TokenInput
	if c.ShouldBindJSON(&input) != nil {
		aiFailure(c, aicontrol.ErrInvalid)
		return
	}
	if value, ok := c.Get(authn.ContextAdminKey); ok {
		if admin, ok := value.(authn.Admin); ok {
			input.AdminID = admin.ID
		}
	}
	token, secret, err := h.service.Store.CreateToken(input)
	if err != nil {
		aiFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, 201, gin.H{"token": token, "secret": secret})
}
func (h *AIHandler) revoke(c *gin.Context) {
	if err := h.service.Store.Revoke(c.Param("tokenId")); err != nil {
		aiFailure(c, err)
		return
	}
	Success(c, 200, gin.H{"revoked": true})
}
func (h *AIHandler) history(c *gin.Context) {
	items, err := h.service.Store.History()
	if err != nil {
		aiFailure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, 200, gin.H{"items": items})
}
func (h *AIHandler) call(c *gin.Context) {
	value, err := h.service.Store.GetCall(c.Param("callId"))
	if err != nil {
		NotFound(c)
		return
	}
	c.Header("Cache-Control", "no-store")
	Success(c, 200, value)
}
func (h *AIHandler) chat(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	var input aicontrol.ChatInput
	if c.ShouldBindJSON(&input) != nil {
		aiFailure(c, aicontrol.ErrInvalid)
		return
	}
	release, err := h.service.Acquire()
	if err != nil {
		aiFailure(c, err)
		return
	}
	defer release()
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-store, no-transform")
	c.Header("X-Accel-Buffering", "no")
	c.Status(200)
	emit := func(event aicontrol.ChatEvent) {
		data, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", data)
		c.Writer.Flush()
	}
	input.Principal = h.principal(c)
	ctx, cancel := context.WithCancel(aicontrol.WithRequestMetadata(c.Request.Context(), aicontrol.RequestAccess(c.Request, c.ClientIP())))
	defer cancel()
	events := make(chan aicontrol.ChatEvent)
	type completion struct {
		out aicontrol.ChatOutput
		err error
	}
	done := make(chan completion, 1)
	go func() {
		out, err := h.service.Chat(ctx, input, func(event aicontrol.ChatEvent) {
			select {
			case events <- event:
			case <-ctx.Done():
			}
		})
		done <- completion{out: out, err: err}
	}()
	// Active requests alone get a heartbeat. A slow model must not leave the
	// SSE connection idle until a reverse proxy silently disconnects it.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-events:
			emit(event)
		case <-heartbeat.C:
			if _, err := fmt.Fprint(c.Writer, ": keepalive\n\n"); err != nil {
				return
			}
			c.Writer.Flush()
		case result := <-done:
			if result.err != nil {
				emit(aicontrol.ChatEvent{Type: "error", Error: result.err.Error()})
				return
			}
			// Full receipts were streamed individually; keep completion small.
			for i := range result.out.Calls {
				result.out.Calls[i].Data = nil
			}
			emit(aicontrol.ChatEvent{Type: "complete", Result: &result.out})
			return
		}
	}
}
func aiFailure(c *gin.Context, err error) {
	status := http.StatusUnprocessableEntity
	switch {
	case errors.Is(err, aicontrol.ErrDenied):
		status = 403
	case errors.Is(err, aicontrol.ErrConflict):
		status = 409
	case errors.Is(err, aicontrol.ErrBusy):
		status = 429
	case errors.Is(err, aicontrol.ErrUnavailable):
		status = 503
	}
	Failure(c, status, "AI_REQUEST_FAILED", err.Error(), nil)
}
