package tmux

import (
	"log"
	"strings"

	"github.com/GianlucaP106/gotmux/gotmux"
)

// TmuxSession 表示一个tmux会话
type TmuxSession struct {
	Name  string `json:"name"`  // 会话名称
	State string `json:"state"` // 会话状态
}

// ListAllSessions 列出所有tmux会话
func ListAllSessions() ([]TmuxSession, error) {
	log.Printf("[TMUX] 开始列出所有tmux会话")

	// 初始化tmux客户端
	tmux, err := gotmux.DefaultTmux()
	if err != nil {
		log.Printf("[TMUX][错误] 初始化tmux失败: %v", err)
		return nil, err
	}

	// 列出所有会话
	sessions, err := tmux.ListSessions()
	if err != nil {
		// 检查错误是否是因为没有tmux会话
		if strings.Contains(err.Error(), "failed to list sessions") {
			log.Printf("[TMUX] 没有运行中的tmux会话")
			return []TmuxSession{}, nil
		}
		log.Printf("[TMUX][错误] 获取tmux会话列表失败: %v", err)
		return nil, err
	}

	// 转换为TmuxSession结构
	result := make([]TmuxSession, 0, len(sessions))
	for _, session := range sessions {
		state := "running"
		if session.Attached > 0 {
			state = "attached"
		}
		result = append(result, TmuxSession{
			Name:  session.Name,
			State: state,
		})
	}

	log.Printf("[TMUX] 已列出所有tmux会话，共 %d 个", len(result))
	return result, nil
}
