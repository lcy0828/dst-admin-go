package gamelog

// LogMessage 表示发送给客户端的日志消息
type LogMessage struct {
	Type      string `json:"type"`      // 消息类型：log, system, rule
	Content   string `json:"content"`   // 消息内容
	Highlight bool   `json:"highlight"` // 是否高亮
	Color     string `json:"color"`     // 高亮颜色
	Timestamp int64  `json:"timestamp"` // 时间戳
}

// RuleOperation 表示规则操作请求
type RuleOperation struct {
	Action string   `json:"action"`  // 操作类型：add, update, delete, enable, disable, list
	Rule   *LogRule `json:"rule"`    // 规则对象（用于add和update操作）
	RuleID string   `json:"rule_id"` // 规则ID（用于delete, enable, disable操作）
}

// RuleResponse 表示规则操作响应
type RuleResponse struct {
	Success bool       `json:"success"` // 操作是否成功
	Message string     `json:"message"` // 操作结果消息
	Rules   []*LogRule `json:"rules"`   // 规则列表（用于list操作）
}
