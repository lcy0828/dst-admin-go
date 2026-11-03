package gamenotifications

import (
	"context"
	"errors"
	"fmt"
	"log"
)

// AnnounceGameUpdate announces the delay chosen for this update. The update
// coordinator owns the countdown, so delivery failure never shortens it and
// room notification preferences cannot override the administrator's choice.
func (s *Service) AnnounceGameUpdate(ctx context.Context, roomIDs []string, seconds int, jobID string) error {
	if seconds < 0 || seconds > 360 {
		return errors.New("停服等待时间必须在 0 到 360 秒之间")
	}
	message := "游戏即将更新，房间会暂时停止，完成后请重新连接。"
	if seconds > 0 {
		message = fmt.Sprintf("游戏将在 %d 秒后更新，房间会暂时停止，请前往安全位置；完成后请重新连接。", seconds)
	}
	source, _, _ := lifecycleMessage("restart", "game_update")
	seen := make(map[string]bool)
	for _, roomID := range roomIDs {
		if roomID == "" || seen[roomID] {
			continue
		}
		seen[roomID] = true
		if _, err := s.sendDirect(ctx, roomID, message, source, jobID); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("[GameNotification] game update announcement room=%s: %v", roomID, err)
		}
	}
	return ctx.Err()
}
