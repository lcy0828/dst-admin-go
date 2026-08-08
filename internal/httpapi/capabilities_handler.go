package httpapi

import (
	"net/http"

	"dont/internal/capabilities"

	"github.com/gin-gonic/gin"
)

func Capabilities(report capabilities.Report) gin.HandlerFunc {
	return func(c *gin.Context) {
		Success(c, http.StatusOK, report)
	}
}

func CapabilitiesProvider(provider func() capabilities.Report) gin.HandlerFunc {
	return func(c *gin.Context) {
		Success(c, http.StatusOK, provider())
	}
}

func SetupChecks(provider func() capabilities.Readiness) gin.HandlerFunc {
	return func(c *gin.Context) {
		Success(c, http.StatusOK, provider())
	}
}
