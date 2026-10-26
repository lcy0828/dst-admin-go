package httpapi

import (
	"context"
	"dont/internal/agents"
	"dont/internal/mods"
	"github.com/gin-gonic/gin"
	"net/http"
)

func (h *ModHandler) ConfigureRuntimeTargets(read func() ([]agents.RuntimeTarget, error)) {
	h.runtimeTargets = read
}

// Resolve only registered candidates. Offline registrations remain candidates;
// choosing the only online node would silently read the wrong installation.
func (h *ModHandler) modDetailScope(c *gin.Context) (string, string, bool) {
	target, installation := c.Query("targetId"), c.Query("installationId")
	if target != "" && installation != "" {
		return target, installation, true
	}
	candidates := []mods.InstallationScope{}
	if room := c.Query("roomId"); room != "" {
		reader, ok := h.placement.(interface {
			LocalModTargets(context.Context, string) ([]mods.InstallationScope, error)
		})
		if !ok {
			Failure(c, 422, "MOD_TARGET_REQUIRED", "请指定模组安装位置", nil)
			return "", "", false
		}
		var err error
		candidates, err = reader.LocalModTargets(c.Request.Context(), room)
		if err != nil {
			modFailure(c, err)
			return "", "", false
		}
	} else if h.runtimeTargets != nil {
		targets, err := h.runtimeTargets()
		if err != nil {
			modFailure(c, err)
			return "", "", false
		}
		for _, node := range targets {
			for _, install := range node.Installations {
				candidates = append(candidates, mods.InstallationScope{TargetID: node.ID, InstallationID: install.ID})
			}
		}
	}
	filtered := []mods.InstallationScope{}
	for _, candidate := range candidates {
		if (target == "" || candidate.TargetID == target) && (installation == "" || candidate.InstallationID == installation) {
			filtered = append(filtered, candidate)
		}
	}
	if len(filtered) == 1 {
		return filtered[0].TargetID, filtered[0].InstallationID, true
	}
	Failure(c, http.StatusUnprocessableEntity, "MOD_TARGET_REQUIRED", "无法唯一确定模组安装位置，请选择运行节点和安装实例", gin.H{"candidates": filtered})
	return "", "", false
}
