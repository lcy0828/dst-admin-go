package httpapi

import (
	"github.com/gin-gonic/gin"
	"net/http"
)

func (h *AgentHandler) setRuntimeTargetDisplayAddress(c *gin.Context) {
	var input struct {
		DisplayAddress *string `json:"displayAddress"`
	}
	if c.ShouldBindJSON(&input) != nil || input.DisplayAddress == nil {
		Failure(c, http.StatusBadRequest, "INVALID_DISPLAY_ADDRESS", "请填写单个 IP 或主机名；留空使用自动发现的地址", nil)
		return
	}
	item, err := h.service.SetRuntimeTargetDisplayAddress(c.Param("targetId"), *input.DisplayAddress)
	if err != nil {
		agentFailure(c, err)
		return
	}
	Success(c, http.StatusOK, item)
}
