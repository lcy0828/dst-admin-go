package tmux

import (
	"strings"
)

// SessionNameParts 会话名称解析结果
type SessionNameParts struct {
	Valid       bool   // 是否有效
	Prefix      string // 前缀
	ClusterName string // 存档名
	ShardName   string // 世界名
}

// ParseSessionName 解析会话名称
// 会话名称格式为: dstserver_存档名_世界名
func ParseSessionName(sessionName string) SessionNameParts {
	parts := strings.Split(sessionName, "_")
	if len(parts) < 3 || parts[0] != "dstserver" {
		return SessionNameParts{
			Valid: false,
		}
	}

	return SessionNameParts{
		Valid:       true,
		Prefix:      parts[0],
		ClusterName: parts[1],
		ShardName:   parts[2],
	}
}
