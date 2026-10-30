package httpapi

import (
	"dont/internal/aicontrol"
	"dont/internal/apicontract"
	"dont/internal/authn"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

func (h *AIHandler) registerBot(v2 *gin.RouterGroup) {
	v2.GET("/bot/capabilities", h.bot("listBotCapabilities"))
	v2.GET("/rooms/:roomId/bot/queries/:capabilityId", h.bot("queryBotCapability"))
	v2.POST("/rooms/:roomId/bot/actions/:capabilityId", h.bot("runBotCapability"))
	v2.GET("/rooms/:roomId/bot/requests/:requestId", h.bot("getBotCapabilityResult"))
}
func (h *AIHandler) bot(id string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		value, ok := c.Get(authn.ContextAdminKey)
		admin, valid := value.(authn.Admin)
		if !ok || !valid || admin.ID < 1 {
			aiFailure(c, aicontrol.ErrDenied)
			return
		}
		p := h.principal(c)
		if c.GetHeader("Authorization") == "" {
			p = aicontrol.Principal{ID: fmt.Sprintf("admin:%d", admin.ID), AdminID: admin.ID}
		}
		if p.ID == "" || p.AdminID != admin.ID {
			aiFailure(c, aicontrol.ErrDenied)
			return
		}
		in := apicontract.Input{Path: map[string]any{}, Query: map[string]any{}}
		for _, k := range []string{"roomId", "capabilityId", "requestId"} {
			if v := c.Param(k); v != "" {
				in.Path[k] = v
			}
		}
		fail := func() { aiFailure(c, aicontrol.ErrInvalid) }
		if id == "runBotCapability" {
			if c.Request.URL.RawQuery != "" {
				fail()
				return
			}
			raw, e := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 12<<10))
			if e != nil {
				fail()
				return
			}
			body, e := aicontrol.StrictBotBodyJSON(raw)
			if e != nil {
				fail()
				return
			}
			in.Body = body
			in.RequestID = c.GetHeader("Idempotency-Key")
		} else {
			if c.Request.ContentLength != 0 || len(c.Request.TransferEncoding) != 0 || c.Request.Body != nil && c.Request.Body != http.NoBody || len(c.Request.URL.RawQuery) > 30<<10 {
				fail()
				return
			}
			values, e := url.ParseQuery(c.Request.URL.RawQuery)
			if e != nil {
				fail()
				return
			}
			allowed := map[string]bool{}
			if id == "listBotCapabilities" {
				for _, k := range []string{"protocolVersion", "knownRevision", "cursor", "limit"} {
					allowed[k] = true
				}
			}
			if id == "queryBotCapability" {
				for _, k := range []string{"contractVersion", "schemaRevision", "requestId", "parameters"} {
					allowed[k] = true
				}
			}
			for k, vs := range values {
				if !allowed[k] || len(vs) != 1 {
					fail()
					return
				}
				if k == "limit" || k == "protocolVersion" || k == "contractVersion" {
					n, e := strconv.Atoi(vs[0])
					if e != nil {
						fail()
						return
					}
					in.Query[k] = n
				} else {
					in.Query[k] = vs[0]
				}
			}
		}
		catalog, e := apicontract.Default()
		if e != nil {
			aiFailure(c, e)
			return
		}
		op, ok := catalog.Operation(id)
		if !ok {
			fail()
			return
		}
		raw, e := json.Marshal(in)
		if e != nil {
			fail()
			return
		}
		in, e = op.Validate(raw)
		if e != nil {
			fail()
			return
		}
		ctx := aicontrol.WithRequestMetadata(c.Request.Context(), aicontrol.RequestAccess(c.Request, c.ClientIP()))
		out, e := h.service.BotAPI(ctx, p, id, in)
		if e != nil {
			aiFailure(c, e)
			return
		}
		Success(c, 200, out)
	}
}
