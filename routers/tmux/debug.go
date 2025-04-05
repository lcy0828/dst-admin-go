package tmux

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/GianlucaP106/gotmux/gotmux"
	"github.com/gin-gonic/gin"
)

// DebugTmuxSessions 输出gotmux可以返回的session的所有值
func DebugTmuxSessions(c *gin.Context) {
	log.Printf("[DEBUG] 开始调试gotmux会话信息")

	// 初始化tmux客户端
	tmux, err := gotmux.DefaultTmux()
	if err != nil {
		log.Printf("[DEBUG][错误] 初始化tmux失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "初始化tmux失败: " + err.Error(),
		})
		return
	}

	// 列出所有会话
	sessions, err := tmux.ListSessions()
	if err != nil {
		log.Printf("[DEBUG][错误] 获取tmux会话列表失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "获取tmux会话列表失败: " + err.Error(),
		})
		return
	}

	// 详细记录每个会话的所有字段
	log.Printf("[DEBUG] 找到 %d 个tmux会话", len(sessions))

	for i, session := range sessions {
		// 将会话信息转换为JSON以便于日志输出
		sessionJSON, err := json.MarshalIndent(session, "", "  ")
		if err != nil {
			log.Printf("[DEBUG][错误] 序列化会话信息失败: %v", err)
			continue
		}

		log.Printf("[DEBUG] 会话 #%d: %s", i+1, string(sessionJSON))

		// 尝试获取更多会话信息
		detailedSession, err := tmux.GetSessionByName(session.Name)
		if err != nil {
			log.Printf("[DEBUG][错误] 获取会话详细信息失败: %v", err)
			continue
		}

		detailedJSON, err := json.MarshalIndent(detailedSession, "", "  ")
		if err != nil {
			log.Printf("[DEBUG][错误] 序列化详细会话信息失败: %v", err)
			continue
		}

		log.Printf("[DEBUG] 会话 #%d 详细信息: %s", i+1, string(detailedJSON))

		// 尝试获取会话的窗口信息
		// 注意：gotmux库不直接提供GetWindowsInSession方法
		// 我们可以使用tmux命令行来获取窗口信息
		output, err := tmux.Command("list-windows", "-t", session.Name, "-F", "#{window_id} #{window_name} #{window_active}")
		if err != nil {
			log.Printf("[DEBUG][错误] 获取会话窗口信息失败: %v", err)
			continue
		}

		log.Printf("[DEBUG] 会话 #%d 窗口信息: %s", i+1, output)
	}

	// 返回会话信息
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "已输出gotmux会话信息到日志",
		"data":   sessions,
	})
}
