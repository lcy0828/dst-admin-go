package aicontrol

// Receipts can contain redacted or truncated business values. Their wire schema
// describes that stable envelope; complete API business schemas are separate
// discovery documentation. Validation must never erase a successful mutation's
// receipt merely because its large result was shortened.
func receiptOutputSchema() map[string]any {
	return map[string]any{
		"type": "object", "required": []string{"callId", "status"},
		"properties": map[string]any{
			"callId": map[string]any{"type": "string", "description": "操作回执编号，可用 getAICall/call_result 查询。"},
			"status": map[string]any{"type": "string", "enum": []string{"executing", "succeeded", "failed", "uncertain"}, "description": "工具调用状态。succeeded 不表示后台任务完成；uncertain 时先核实实际结果，不要盲目重发写操作。"},
			"data":   map[string]any{"description": "工具数据。API 工具通常含 operationId、httpStatus、data（业务结果）；失败含 error，下载或订阅含 resource。数据可能已脱敏或含 _aiTruncated，此时缺失字段不代表空值或成功。"},
		},
	}
}
