package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

const requestIDKey = "dst_admin.request_id"

type responseMeta struct {
	RequestID string `json:"requestId"`
}

type errorBody struct {
	Code    string      `json:"code"`
	Message string      `json:"message"`
	Details interface{} `json:"details,omitempty"`
}

func Success(c *gin.Context, status int, data interface{}) {
	c.JSON(status, gin.H{"data": data, "error": nil, "meta": responseMeta{RequestID: RequestID(c)}})
}

func Failure(c *gin.Context, status int, code, message string, details interface{}) {
	c.AbortWithStatusJSON(status, gin.H{
		"data":  nil,
		"error": errorBody{Code: code, Message: message, Details: details},
		"meta":  responseMeta{RequestID: RequestID(c)},
	})
}

func RequestID(c *gin.Context) string {
	value, _ := c.Get(requestIDKey)
	requestID, _ := value.(string)
	return requestID
}

func NotFound(c *gin.Context) {
	Failure(c, http.StatusNotFound, "NOT_FOUND", "请求的资源不存在", nil)
}
