package aicontrol

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Evidence requirements are deliberately domain-specific. Ordinary conversation
// and explanations need no platform calls. These checks are a backstop for
// skipped queries, not a semantic validator of every sentence a model writes.
type turnEvidence struct {
	required []string
	observed map[string]bool
	repair   bool
	question string
}

func newTurnEvidence(input ChatInput) *turnEvidence {
	q := strings.ToLower(strings.TrimSpace(input.Messages[len(input.Messages)-1].Content))
	e := &turnEvidence{observed: map[string]bool{}, question: q}
	if containsAny(q, "你能做什么", "你可以做什么", "有哪些功能", "what can you do", "your capabilities") {
		return e
	}
	explanation := containsAny(q, "是什么意思", "怎么理解", "解释一下", "如何配置", "如何使用", "怎么设置", "举个例子", "what does", "what is", "how to", "explain")
	execution := containsAny(q, "帮我查", "帮我看", "帮我执行", "执行一下", "执行这", "运行一下", "用控制台读", "run this", "execute this", "check now")
	if explanation && !execution && !containsAny(q, "当前", "现在", "最近", "current", "recent", "now") {
		return e
	}
	domains := []struct {
		name  string
		words []string
	}{
		{"console", []string{"用控制台", "执行lua", "执行 lua", "运行lua", "运行 lua", "execute lua", "run lua", "run this code", "console to", "执行这段", "执行一下print", "执行一下 print"}},
		{"state", []string{"第几天", "多少天", "天数", "什么季节", "季节", "天气", "world.state", "world state", "what day", "season", "weather", "cycles"}},
		{"logs", []string{"日志", "log"}},
		{"performance", []string{"性能", "卡不卡", "占用", "cpu", "内存", "performance", "lag", "memory"}},
		{"players", []string{"玩家", "多少个用户", "在线人数", "players", "player count"}},
		{"mods", []string{"模组", "mods", "mod list"}},
		{"backups", []string{"备份", "backup"}},
		{"automation", []string{"定时任务", "自动化", "automation", "scheduled tasks"}},
	}
	inspection := containsAny(q, "看", "查", "读", "现在", "当前", "最近", "有没有", "是否", "了吗", "多少", "几", "什么", "开了", "开着", "正常", "运行", "怎么样", "如何", "how is", "how are", "check", "show", "read", "current", "recent", "is ", "are ", "how many", "what ")
	for _, domain := range domains {
		if containsAny(q, domain.words...) && (inspection || execution) {
			e.required = append(e.required, domain.name)
		}
	}
	// A literal console read is answered by that invocation's receipt, not a
	// second world-state read that could be mistaken for execution.
	if containsAny(q, "控制台", "console") && (execution || containsAny(q, "执行", "运行", "run ", "execute ")) {
		e.required = []string{"console"}
	}
	if len(e.required) == 0 && inspection && containsAny(q, "状态", "运行正常", "正常运行", "status", "running", "healthy") {
		e.required = []string{"status"}
	}
	return e
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func (e *turnEvidence) record(tool string, result Invocation, err error) {
	if err != nil || result.Status != "succeeded" {
		return
	}
	// Discovery, room catalogs and previous receipts never satisfy a current
	// observation. Typed OpenAPI tools are mapped to their actual domain.
	domains := map[string][]string{
		"status":      {"room_status", "api_getRoomRuntime", "api_getRoomRuntimeOverview"},
		"state":       {"world_state", "api_listWorldStates", "api_refreshWorldState", "api_refreshWorldStates"},
		"logs":        {"world_logs", "api_getWorldLog", "api_getRoomLogs"},
		"performance": {"room_performance", "api_getRoomRuntimeOverview", "api_getNodeResources"},
		"players":     {"room_players", "api_listRoomPlayers", "api_getRoomPlayer", "api_getRoomPlayerInventory"},
		"mods":        {"room_mods", "api_listRoomMods", "api_getRoomModProfile"},
		"backups":     {"room_backups", "api_listRoomBackupSchedules", "api_getRoomBackupPolicy", "api_configureRoomBackupSchedule", "api_listRoomBackups", "api_listDistributedBackupSets"},
		"automation":  {"room_automation", "api_listAutomationTasks", "api_getAutomationTask"},
		"console":     {"world_console", "api_executeRawRoomCommand"},
	}
	for domain, names := range domains {
		for _, name := range names {
			if tool == name {
				e.observed[domain] = true
			}
		}
	}
}

func (e *turnEvidence) missing() []string {
	var missing []string
	for _, domain := range e.required {
		if !e.observed[domain] {
			missing = append(missing, domain)
		}
	}
	return missing
}

func (e *turnEvidence) permitsClarification(input ChatInput, content string) bool {
	// A short target clarification without observations remains valid. A question
	// appended to a purported finding must not waive evidence requirements.
	text := strings.TrimSpace(content)
	return input.RoomID == "" && len([]rune(text)) <= 160 && containsAny(text, "哪个房间", "哪个世界", "哪一个", "which room", "which world", "which one") &&
		!containsAny(strings.ToLower(text), "运行中", "已停止", "正常", "第", "天数", "没有错误", "已检查", "我查", "running", "healthy", "stopped", "checked", "error-free")
}

func (e *turnEvidence) repairPrompt() string {
	return "Current-turn evidence is missing for: " + strings.Join(e.missing(), ", ") + ". Reuse identifiers only; old answers/receipts do not establish current facts. Call the matching domain tool now (discover typed APIs if needed), then answer from its actual result. Read-only inspection requires no approval. If a query failed, state that it failed; do not claim health or invent values. Do not replay any mutation already submitted."
}

func (e *turnEvidence) unavailable(locale string) string {
	missing := e.missing()
	if locale == "en-US" {
		return "This turn did not obtain current evidence for " + strings.Join(missing, ", ") + ". I cannot confirm these results from earlier conversation. Any returned operation receipts remain available below; no operation has been retried."
	}
	labels := map[string]string{"state": "世界天数、季节或天气", "logs": "日志", "performance": "性能", "players": "玩家数据", "mods": "模组配置", "backups": "备份配置或记录", "automation": "定时任务", "console": "本次控制台执行结果", "status": "运行状态"}
	values := make([]string, 0, len(missing))
	for _, domain := range missing {
		values = append(values, labels[domain])
	}
	return "本轮未取得" + strings.Join(values, "、") + "的有效查询结果，暂时无法确认，不能沿用之前对话中的数值或结论。已返回的操作回执保留在下方。"
}

var namedShardPattern = regexp.MustCompile(`(?i)\b(caves|master)\b|洞穴|地面|主世界|主分片|这个分片|该分片|这个世界|该世界`)

// A request naming one shard cannot fall through to an all-room lifecycle
// action. Correct API calls specify one world ID from the room catalog.
func checkChatWorldScope(question, tool string, raw json.RawMessage) error {
	if !namedShardPattern.MatchString(question) || containsAny(strings.ToLower(question), "整个房间", "全部世界", "所有世界", "全部分片", "所有分片", "whole room", "all worlds", "all shards", "master和caves", "master 和 caves", "地面和洞穴", "master and caves") {
		return nil
	}
	if tool == "room_control" {
		return fmt.Errorf("AI_WORLD_SCOPE_REQUIRED: use api_runRoomAction with body.worldIds containing only the requested world's returned ID")
	}
	if tool != "api_runRoomAction" && tool != "api_runBatchRoomAction" {
		return nil
	}
	var input struct {
		Body struct {
			WorldIDs []string `json:"worldIds"`
		} `json:"body"`
	}
	if json.Unmarshal(raw, &input) != nil || tool == "api_runBatchRoomAction" || len(input.Body.WorldIDs) != 1 {
		return fmt.Errorf("AI_WORLD_SCOPE_REQUIRED: a named-world action must specify exactly one body.worldIds entry; do not expand it to the whole room")
	}
	return nil
}
