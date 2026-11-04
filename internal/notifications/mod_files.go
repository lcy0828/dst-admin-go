package notifications

import (
	"sort"
	"time"
)

// ObserveModFiles is scoped by the applied enabled configuration, not by every
// room that happens to share the installation. It never asserts activation.
func (s *Service) ObserveModFiles(jobID, roomID string, modIDs []string, complete bool, at time.Time) {
	if jobID == "" || roomID == "" || len(modIDs) == 0 {
		return
	}
	ids := append([]string(nil), modIDs...)
	sort.Strings(ids)
	if len(ids) > 100 {
		ids = ids[:100]
	}
	status, severity := "prepared", "info"
	message := "相关模组文件已更新；运行中的世界需要重启后加载，已停止的世界下次启动时加载。"
	if !complete {
		status, severity = "uncertain", "warning"
		message = "相关模组文件更新未全部确认，请在面板查看任务详情；当前游戏是否使用新版仍需核对。"
	}
	s.fact(Event{ID: "mod-files:" + jobID + ":" + roomID, Kind: "mod.activation", Category: "updates", RoomID: roomID, JobID: jobID, Status: status, Severity: severity, Title: "模组文件更新", Message: message, OccurredAt: at, Facts: &Facts{Action: "mod.update", OperationID: jobID, ModIDs: ids, RequiresRestart: complete}})
}
