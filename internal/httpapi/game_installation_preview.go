package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"dont/internal/gameinstall"
	"dont/internal/gameupdate"
	"github.com/gin-gonic/gin"
)

type gameInstallationPreview struct {
	Mode         string                      `json:"mode"`
	Ready        bool                        `json:"ready"`
	Installation gameinstall.Installation    `json:"installation"`
	Plan         *gameupdate.ReleasePlan     `json:"plan,omitempty"`
	Blockers     []gameupdate.ReleaseBlocker `json:"blockers"`
}

// Preview never stops worlds, creates jobs or downloads game content.
func (h *GameInstallationHandler) preview(c *gin.Context) {
	var input struct {
		TargetID       string `json:"targetId" binding:"required"`
		InstallationID string `json:"installationId" binding:"required"`
		RestartRunning *bool  `json:"restartRunning"`
	}
	if c.ShouldBindJSON(&input) != nil {
		Failure(c, http.StatusBadRequest, "INVALID_GAME_INSTALLATION", "请选择机器和安装位置", nil)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), gameReleasePreviewTimeout)
	defer cancel()
	installation, err := h.service.Check(ctx, input.TargetID, input.InstallationID)
	if err != nil {
		gameInstallationFailure(c, err)
		return
	}
	value := gameInstallationPreview{Mode: "install", Installation: installation, Blockers: []gameupdate.ReleaseBlocker{}}
	block := func(code, message string) {
		value.Blockers = append(value.Blockers, gameupdate.ReleaseBlocker{Code: code, Message: message, TargetID: input.TargetID, InstallationID: input.InstallationID})
	}
	if !installation.CanInstall {
		block("INSTALLATION_UNSUPPORTED", installation.Reason)
	}
	if installation.UsageState != "idle" && installation.UsageState != "running" {
		block("USAGE_UNAVAILABLE", "无法确认该安装的使用状态，请重新检查；旧版 Agent 需先升级")
	}
	if len(value.Blockers) == 0 && installation.Installed {
		plan, err := h.releases.Preview(ctx, gameupdate.ReleasePreviewRequest{
			Installations: []gameupdate.ReleaseInstallationSelection{{TargetID: input.TargetID, InstallationID: input.InstallationID}},
			Policy:        gameupdate.ReleasePolicyInput{Validate: true, RestartRunning: input.RestartRunning},
		})
		if err != nil && !errors.Is(err, gameupdate.ErrReleaseInstallationUnused) {
			gameReleaseFailure(c, err)
			return
		}
		if err == nil {
			value.Mode, value.Plan = "release", &plan
			value.Blockers = append(value.Blockers, plan.Blockers...)
			value.Blockers = append(value.Blockers, h.checkReleaseUsage(ctx, installation, plan)...)
		}
	}
	if value.Mode == "install" && installation.UsageState == "running" {
		block("UNMANAGED_GAME_RUNNING", "此目录正在被游戏进程使用，但没有可自动停止的受管更新计划。请先停止对应世界，再重新检查")
	}
	value.Ready = len(value.Blockers) == 0 && (value.Plan == nil || value.Plan.Ready)
	c.Header("Cache-Control", "no-store")
	Success(c, http.StatusOK, value)
}

// Check every connected installation before the user confirms downtime. This
// stays demand-driven and bounded even for rooms spanning several Agents.
func (h *GameInstallationHandler) checkReleaseUsage(ctx context.Context, selected gameinstall.Installation, plan gameupdate.ReleasePlan) []gameupdate.ReleaseBlocker {
	results := make([][]gameupdate.ReleaseBlocker, len(plan.Installations))
	slots := make(chan struct{}, 2)
	var wg sync.WaitGroup
	for index, target := range plan.Installations {
		index, target := index, target
		wg.Add(1)
		go func() {
			defer wg.Done()
			block := func(code, message string) {
				results[index] = append(results[index], gameupdate.ReleaseBlocker{Code: code, Message: message, TargetID: target.TargetID, InstallationID: target.InstallationID})
			}
			current := selected
			if current.TargetID != target.TargetID || current.InstallationID != target.InstallationID {
				select {
				case slots <- struct{}{}:
					defer func() { <-slots }()
				case <-ctx.Done():
					block("USAGE_UNAVAILABLE", "安装占用检查超时，请重新检查")
					return
				}
				var err error
				current, err = h.service.Check(ctx, target.TargetID, target.InstallationID)
				if err != nil {
					block("USAGE_UNAVAILABLE", "无法确认关联安装的使用状态: "+err.Error())
					return
				}
			}
			if current.UsageState != "idle" && current.UsageState != "running" {
				block("USAGE_UNAVAILABLE", "无法确认关联安装的使用状态，请重新检查；旧版 Agent 需先升级")
			} else if current.RunningProcesses > target.RunningShards {
				block("UNMANAGED_GAME_RUNNING", "此目录还被未纳入更新计划的游戏进程使用，请先在运行机器上停止这些进程，再重新检查")
			}
		}()
	}
	wg.Wait()
	var blockers []gameupdate.ReleaseBlocker
	for _, values := range results {
		blockers = append(blockers, values...)
	}
	return blockers
}
