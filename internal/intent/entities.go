package intent

import (
	"regexp"
	"strings"
)

// Context supplies optional live room context (such as online/known player
// names) so entity tagging never strips a prefix that is actually part of a
// real player's nickname.
type Context struct {
	KnownPlayers []string
	KnownWorlds  []string
}

// TaggedQuery holds normalized text and structured entities extracted prior to
// capability ranking and slot filling.
type TaggedQuery struct {
	Raw           string
	Normalized    string
	Compact       string
	Template      string
	Player        string
	Item          string
	Count         int
	Days          int
	Window        string
	WorldRole     string
	Enabled       *bool
	Backup        *bool
	HasPerfSignal bool
}

var (
	// "给玩家lcy 发五个草", "给玩家lcy发5个草", "给 lcy 发 10 个齿轮"
	giveItemPattern = regexp.MustCompile(`^(?:给|帮|向|为)\s*(.+?)\s*(?:发放?|送|刷|拿|塞)\s*([0-9零一二两三四五六七八九十百]+)\s*(?:个|组|份|根|块|把|朵|棵|只|张|颗)?\s*(.+)$`)
	// "发五个草给玩家lcy", "发 5 个草给 lcy"
	giveItemReversePattern = regexp.MustCompile(`^(?:发放?|送|刷)\s*([0-9零一二两三四五六七八九十百]+)\s*(?:个|组|份|根|块|把|朵|棵|只|张|颗)?\s*(.+?)\s*(?:给|到)\s*(.+)$`)
	// "给玩家lcy开启无敌模式", "帮 lcy 关闭创造模式"
	playerModePattern = regexp.MustCompile(`^(?:给|帮|为|让)\s*(.+?)\s*(开启|打开|启用|开|关闭|关掉|取消|禁用|关)\s*(无敌|上帝|神仙|创造|制作|全配方)\s*(?:模式)?$`)
	// "复活玩家lcy", "复活 lcy", "治疗玩家lcy", "杀死玩家lcy", "踢出玩家lcy"
	verbPlayerPattern = regexp.MustCompile(`^(?:复活|救活|拉起|治疗|回血|杀死|击杀|处死|踢出|踢掉|封禁|拉黑|解封|重置角色|换人|查看背包|查背包|查玩家|查看玩家)\s*(?:一下\s*)?([A-Za-z0-9_\p{Han}\s-]{1,32})$`)
	// "跳过一天", "跳过 3 天", "快进三天"
	skipDaysPattern = regexp.MustCompile(`(?:跳过|快进|推进|往后跳)\s*([0-9零一二两三四五六七八九十百]+)\s*(?:个?\s*天|日)`)
	// Time windows
	window7dRe  = regexp.MustCompile(`(?:最近|过去|近)?\s*(?:7|七)\s*天|(?:最近|过去|近)?\s*(?:1|一)\s*周|一周内|七天内|7d`)
	window24hRe = regexp.MustCompile(`(?:最近|过去|近)?\s*(?:1|一)\s*天|(?:最近|过去|近)?\s*24\s*小时|一天内|24h|今天|昨日|昨天`)
	window6hRe  = regexp.MustCompile(`(?:最近|过去|近)?\s*(?:6|六)\s*小时|半天|6h`)
	window1hRe  = regexp.MustCompile(`(?:最近|过去|近)?\s*(?:1|一)\s*小时|1h|刚才`)
	perfSigRe   = regexp.MustCompile(`(?i)(?:cpu|内存|rss|帧率|tick|负载|卡不卡|性能指标|性能报表|性能|运行指标|监控指标)`)
)

var nonPlayerNouns = map[string]bool{
	"地图": true, "世界地图": true, "服务器": true, "房间": true, "世界": true,
	"状态": true, "性能": true, "模组": true, "mod": true, "mods": true,
	"备份": true, "存档": true, "白天": true, "黄昏": true, "夜晚": true,
	"春天": true, "夏天": true, "秋天": true, "冬天": true,
}

// TagEntities normalizes raw user text and extracts structured entities.
func TagEntities(raw string, ctx Context) TaggedQuery {
	norm := Normalize(raw)
	comp := Compact(norm)
	out := TaggedQuery{
		Raw:        raw,
		Normalized: norm,
		Compact:    comp,
		Template:   norm,
	}

	if perfSigRe.MatchString(norm) {
		out.HasPerfSignal = true
	}

	switch {
	case window7dRe.MatchString(norm):
		out.Window = "7d"
	case window24hRe.MatchString(norm):
		out.Window = "24h"
	case window6hRe.MatchString(norm):
		out.Window = "6h"
	case window1hRe.MatchString(norm):
		out.Window = "1h"
	}

	if strings.Contains(comp, "不备份") || strings.Contains(comp, "不要备份") || strings.Contains(comp, "无需备份") || strings.Contains(comp, "跳过备份") {
		b := false
		out.Backup = &b
	} else if strings.Contains(comp, "先备份") || strings.Contains(comp, "并备份") {
		b := true
		out.Backup = &b
	}

	if strings.Contains(comp, "洞穴") || strings.Contains(comp, "地洞") || strings.Contains(comp, "caves") {
		out.WorldRole = "caves"
	} else if strings.Contains(comp, "地面") || strings.Contains(comp, "主世界") || strings.Contains(comp, "master") {
		out.WorldRole = "master"
	}

	if m := skipDaysPattern.FindStringSubmatch(norm); len(m) == 2 {
		if n, ok := ParseChineseInt(m[1]); ok && n >= 1 && n <= 100 {
			out.Days = n
			out.Template = skipDaysPattern.ReplaceAllString(norm, "跳过{days}天")
		}
	}

	if m := giveItemPattern.FindStringSubmatch(norm); len(m) == 4 {
		player := cleanPlayerCandidate(m[1], ctx)
		n, ok := ParseChineseInt(m[2])
		item := strings.TrimSpace(m[3])
		if player != "" && ok && n > 0 && item != "" {
			out.Player = player
			out.Count = n
			out.Item = item
			out.Template = "给{player}发{count}个{item}"
			return out
		}
	}

	if m := giveItemReversePattern.FindStringSubmatch(norm); len(m) == 4 {
		n, ok := ParseChineseInt(m[1])
		item := strings.TrimSpace(m[2])
		player := cleanPlayerCandidate(m[3], ctx)
		if player != "" && ok && n > 0 && item != "" {
			out.Player = player
			out.Count = n
			out.Item = item
			out.Template = "发{count}个{item}给{player}"
			return out
		}
	}

	if m := playerModePattern.FindStringSubmatch(norm); len(m) == 4 {
		player := cleanPlayerCandidate(m[1], ctx)
		verb := m[2]
		modeWord := m[3]
		if player != "" {
			out.Player = player
			enabled := !strings.Contains("关闭关掉取消禁用关", verb)
			out.Enabled = &enabled
			out.Template = "给{player}" + verb + modeWord + "模式"
			return out
		}
	}

	if m := verbPlayerPattern.FindStringSubmatch(norm); len(m) == 2 {
		player := cleanPlayerCandidate(m[1], ctx)
		if player != "" && !nonPlayerNouns[strings.ToLower(player)] {
			out.Player = player
		}
	}

	if out.Enabled == nil {
		if strings.Contains(comp, "开启") || strings.Contains(comp, "打开") || strings.Contains(comp, "启用") {
			b := true
			out.Enabled = &b
		} else if strings.Contains(comp, "关闭") || strings.Contains(comp, "关掉") || strings.Contains(comp, "取消") || strings.Contains(comp, "禁用") {
			b := false
			out.Enabled = &b
		}
	}

	return out
}

func cleanPlayerCandidate(raw string, ctx Context) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// If a known player literally has the exact raw name (e.g. "玩家甲"), keep it.
	for _, kp := range ctx.KnownPlayers {
		if strings.EqualFold(strings.TrimSpace(kp), s) {
			return strings.TrimSpace(kp)
		}
	}
	if strings.HasPrefix(s, "玩家") {
		stripped := strings.TrimSpace(strings.TrimPrefix(s, "玩家"))
		if stripped != "" {
			return stripped
		}
	}
	return s
}
