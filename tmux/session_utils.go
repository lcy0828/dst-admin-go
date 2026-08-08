package tmux

import (
	"encoding/base32"
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
	if len(parts) == 4 && parts[0] == "dstserver" && parts[1] == "v2" {
		encoding := base32.StdEncoding.WithPadding(base32.NoPadding)
		cluster, clusterErr := encoding.DecodeString(parts[2])
		shard, shardErr := encoding.DecodeString(parts[3])
		if clusterErr == nil && shardErr == nil && len(cluster) > 0 && len(shard) > 0 {
			return SessionNameParts{Valid: true, Prefix: "dstserver_v2", ClusterName: string(cluster), ShardName: string(shard)}
		}
	}
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

func V2SessionName(clusterName, shardName string) string {
	encoding := base32.StdEncoding.WithPadding(base32.NoPadding)
	return "dstserver_v2_" + encoding.EncodeToString([]byte(clusterName)) + "_" + encoding.EncodeToString([]byte(shardName))
}
