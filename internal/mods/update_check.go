package mods

import (
	"fmt"
	"time"
)

// ModUpdateCheck describes this observation, independently of a pending
// download/restart. Unknown never means current or an empty successful check.
type ModUpdateCheck struct {
	Scope     ModUpdateScope       `json:"scope"`
	CheckedAt time.Time            `json:"checkedAt"`
	Complete  bool                 `json:"complete"`
	Total     int                  `json:"total"`
	Current   int                  `json:"current"`
	Outdated  int                  `json:"outdated"`
	Unknown   int                  `json:"unknown"`
	Local     int                  `json:"local"`
	Items     []ModUpdateCheckItem `json:"items"`
}

type ModUpdateCheckItem struct {
	ModID         string                    `json:"modId"`
	Name          string                    `json:"name"`
	Status        string                    `json:"status"`
	Reason        string                    `json:"reason,omitempty"`
	LatestVersion string                    `json:"latestVersion,omitempty"`
	Targets       []RuntimeModVersionTarget `json:"targets"`
}

func (c *ModUpdateCheck) Message() string {
	if c == nil {
		return "模组更新状态已刷新"
	}
	return fmt.Sprintf("已检查 %d 个工坊模组：%d 个可更新，%d 个已是最新，%d 个暂时无法确认；另有 %d 个本地模组", c.Total, c.Outdated, c.Current, c.Unknown, c.Local)
}
