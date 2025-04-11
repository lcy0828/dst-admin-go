package gamelog

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// RuleRequest 规则请求结构
type RuleRequest struct {
	ArchiveName string   `json:"archive_name"` // 存档名称
	WorldName   string   `json:"world_name"`   // 世界名称
	Rule        *LogRule `json:"rule"`         // 规则对象
	RuleID      string   `json:"rule_id"`      // 规则ID
}

// GroupRequest 分组请求结构
type GroupRequest struct {
	ArchiveName string     `json:"archive_name"` // 存档名称
	WorldName   string     `json:"world_name"`   // 世界名称
	Group       *RuleGroup `json:"group"`        // 分组对象
	GroupID     string     `json:"group_id"`     // 分组ID
}

// TestRequest 测试请求结构
type TestRequest struct {
	ArchiveName string `json:"archive_name"` // 存档名称
	WorldName   string `json:"world_name"`   // 世界名称
	RuleID      string `json:"rule_id"`      // 规则ID
	Line        string `json:"line"`         // 测试行
}

// ExportRequest 导出请求结构
type ExportRequest struct {
	ArchiveName string `json:"archive_name"` // 存档名称
	WorldName   string `json:"world_name"`   // 世界名称
	Description string `json:"description"`  // 描述
}

// ImportRequest 导入请求结构
type ImportRequest struct {
	ArchiveName string          `json:"archive_name"` // 存档名称
	WorldName   string          `json:"world_name"`   // 世界名称
	Data        *ExportRuleData `json:"data"`         // 导入数据
	Overwrite   bool            `json:"overwrite"`    // 是否覆盖
}

// HandleGetRules 获取规则列表
func HandleGetRules(c *gin.Context) {
	// 获取参数
	archiveName := c.Query("archive")
	worldName := c.Query("world")

	if archiveName == "" || worldName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称(archive)和世界名称(world)",
		})
		return
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(archiveName, worldName)

	// 获取规则列表
	rules := ruleManager.GetRules()

	// 返回规则列表
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取规则列表成功",
		"data":   rules,
	})
}

// HandleGetGroups 获取分组列表
func HandleGetGroups(c *gin.Context) {
	// 获取参数
	archiveName := c.Query("archive")
	worldName := c.Query("world")

	if archiveName == "" || worldName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称(archive)和世界名称(world)",
		})
		return
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(archiveName, worldName)

	// 获取分组列表
	groups := ruleManager.GetGroups()

	// 返回分组列表
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取分组列表成功",
		"data":   groups,
	})
}

// HandleGetRulesByGroup 获取指定分组的规则列表
func HandleGetRulesByGroup(c *gin.Context) {
	// 获取参数
	archiveName := c.Query("archive")
	worldName := c.Query("world")
	groupID := c.Query("group_id")

	if archiveName == "" || worldName == "" || groupID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称(archive)、世界名称(world)和分组ID(group_id)",
		})
		return
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(archiveName, worldName)

	// 获取指定分组的规则列表
	rules := ruleManager.GetRulesByGroup(groupID)

	// 返回规则列表
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取分组规则列表成功",
		"data":   rules,
	})
}

// HandleAddRule 添加规则
func HandleAddRule(c *gin.Context) {
	// 解析请求
	var req RuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    fmt.Sprintf("请求参数错误: %v", err),
		})
		return
	}

	// 验证参数
	if req.ArchiveName == "" || req.WorldName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称和世界名称",
		})
		return
	}

	if req.Rule == nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "规则数据不能为空",
		})
		return
	}

	// 生成规则ID
	if req.Rule.ID == "" {
		req.Rule.ID = uuid.New().String()
	}

	// 设置创建和更新时间
	now := time.Now()
	req.Rule.CreatedAt = now
	req.Rule.UpdatedAt = now

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(req.ArchiveName, req.WorldName)

	// 添加规则
	err := ruleManager.AddRule(req.Rule)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("添加规则失败: %v", err),
		})
		return
	}

	// 返回成功
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "添加规则成功",
		"data":   req.Rule,
	})
}

// HandleAddGroup 添加分组
func HandleAddGroup(c *gin.Context) {
	// 解析请求
	var req GroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    fmt.Sprintf("请求参数错误: %v", err),
		})
		return
	}

	// 验证参数
	if req.ArchiveName == "" || req.WorldName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称和世界名称",
		})
		return
	}

	if req.Group == nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "分组数据不能为空",
		})
		return
	}

	// 生成分组ID
	if req.Group.ID == "" {
		req.Group.ID = uuid.New().String()
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(req.ArchiveName, req.WorldName)

	// 添加分组
	err := ruleManager.AddGroup(req.Group)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("添加分组失败: %v", err),
		})
		return
	}

	// 返回成功
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "添加分组成功",
		"data":   req.Group,
	})
}

// HandleUpdateGroup 更新分组
func HandleUpdateGroup(c *gin.Context) {
	// 解析请求
	var req GroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    fmt.Sprintf("请求参数错误: %v", err),
		})
		return
	}

	// 验证参数
	if req.ArchiveName == "" || req.WorldName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称和世界名称",
		})
		return
	}

	if req.Group == nil || req.Group.ID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "分组数据不完整",
		})
		return
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(req.ArchiveName, req.WorldName)

	// 更新分组
	err := ruleManager.UpdateGroup(req.Group)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("更新分组失败: %v", err),
		})
		return
	}

	// 返回成功
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "更新分组成功",
		"data":   req.Group,
	})
}

// HandleDeleteGroup 删除分组
func HandleDeleteGroup(c *gin.Context) {
	// 解析请求
	var req GroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    fmt.Sprintf("请求参数错误: %v", err),
		})
		return
	}

	// 验证参数
	if req.ArchiveName == "" || req.WorldName == "" || req.GroupID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称、世界名称和分组ID",
		})
		return
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(req.ArchiveName, req.WorldName)

	// 删除分组
	err := ruleManager.DeleteGroup(req.GroupID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("删除分组失败: %v", err),
		})
		return
	}

	// 返回成功
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "删除分组成功",
	})
}

// HandleEnableGroup 启用分组
func HandleEnableGroup(c *gin.Context) {
	// 解析请求
	var req GroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    fmt.Sprintf("请求参数错误: %v", err),
		})
		return
	}

	// 验证参数
	if req.ArchiveName == "" || req.WorldName == "" || req.GroupID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称、世界名称和分组ID",
		})
		return
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(req.ArchiveName, req.WorldName)

	// 启用分组
	err := ruleManager.EnableGroup(req.GroupID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("启用分组失败: %v", err),
		})
		return
	}

	// 返回成功
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "启用分组成功",
	})
}

// HandleDisableGroup 禁用分组
func HandleDisableGroup(c *gin.Context) {
	// 解析请求
	var req GroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    fmt.Sprintf("请求参数错误: %v", err),
		})
		return
	}

	// 验证参数
	if req.ArchiveName == "" || req.WorldName == "" || req.GroupID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称、世界名称和分组ID",
		})
		return
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(req.ArchiveName, req.WorldName)

	// 禁用分组
	err := ruleManager.DisableGroup(req.GroupID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("禁用分组失败: %v", err),
		})
		return
	}

	// 返回成功
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "禁用分组成功",
	})
}

// HandleUpdateRule 更新规则
func HandleUpdateRule(c *gin.Context) {
	// 解析请求
	var req RuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    fmt.Sprintf("请求参数错误: %v", err),
		})
		return
	}

	// 验证参数
	if req.ArchiveName == "" || req.WorldName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称和世界名称",
		})
		return
	}

	if req.Rule == nil || req.Rule.ID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "规则数据不完整",
		})
		return
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(req.ArchiveName, req.WorldName)

	// 更新规则
	err := ruleManager.UpdateRule(req.Rule)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("更新规则失败: %v", err),
		})
		return
	}

	// 返回成功
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "更新规则成功",
		"data":   req.Rule,
	})
}

// HandleTestRule 测试规则
func HandleTestRule(c *gin.Context) {
	// 解析请求
	var req TestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    fmt.Sprintf("请求参数错误: %v", err),
		})
		return
	}

	// 验证参数
	if req.ArchiveName == "" || req.WorldName == "" || req.RuleID == "" || req.Line == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称、世界名称、规则ID和测试行",
		})
		return
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(req.ArchiveName, req.WorldName)

	// 测试规则
	result, err := ruleManager.TestRule(req.RuleID, req.Line)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("测试规则失败: %v", err),
		})
		return
	}

	// 返回成功
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "测试规则成功",
		"data":   result,
	})
}

// HandleTestAllRules 测试所有规则
func HandleTestAllRules(c *gin.Context) {
	// 解析请求
	var req TestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    fmt.Sprintf("请求参数错误: %v", err),
		})
		return
	}

	// 验证参数
	if req.ArchiveName == "" || req.WorldName == "" || req.Line == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称、世界名称和测试行",
		})
		return
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(req.ArchiveName, req.WorldName)

	// 测试所有规则
	results, err := ruleManager.TestAllRules(req.Line)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("测试规则失败: %v", err),
		})
		return
	}

	// 返回成功
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "测试所有规则成功",
		"data":   results,
	})
}

// HandleTestLine 测试日志行
func HandleTestLine(c *gin.Context) {
	// 解析请求
	var req TestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    fmt.Sprintf("请求参数错误: %v", err),
		})
		return
	}

	// 验证参数
	if req.ArchiveName == "" || req.WorldName == "" || req.Line == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称、世界名称和测试行",
		})
		return
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(req.ArchiveName, req.WorldName)

	// 测试日志行
	resultLine, showLine, color := ruleManager.TestLine(req.Line)

	// 返回成功
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "测试日志行成功",
		"data": gin.H{
			"result_line": resultLine,
			"show_line":   showLine,
			"color":       color,
		},
	})
}

// HandleExportRules 导出规则
func HandleExportRules(c *gin.Context) {
	// 解析请求
	var req ExportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    fmt.Sprintf("请求参数错误: %v", err),
		})
		return
	}

	// 验证参数
	if req.ArchiveName == "" || req.WorldName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称和世界名称",
		})
		return
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(req.ArchiveName, req.WorldName)

	// 导出规则
	exportData, err := ruleManager.ExportRules(req.Description)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("导出规则失败: %v", err),
		})
		return
	}

	// 返回成功
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "导出规则成功",
		"data":   exportData,
	})
}

// HandleImportRules 导入规则
func HandleImportRules(c *gin.Context) {
	// 解析请求
	var req ImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    fmt.Sprintf("请求参数错误: %v", err),
		})
		return
	}

	// 验证参数
	if req.ArchiveName == "" || req.WorldName == "" || req.Data == nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称、世界名称和导入数据",
		})
		return
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(req.ArchiveName, req.WorldName)

	// 导入规则
	err := ruleManager.ImportRules(req.Data, req.Overwrite)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("导入规则失败: %v", err),
		})
		return
	}

	// 返回成功
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "导入规则成功",
		"data":   ruleManager.GetRules(),
	})
}

// HandleGetStats 获取统计信息
func HandleGetStats(c *gin.Context) {
	// 获取参数
	archiveName := c.Query("archive")
	worldName := c.Query("world")

	if archiveName == "" || worldName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称(archive)和世界名称(world)",
		})
		return
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(archiveName, worldName)

	// 获取统计信息
	stats := ruleManager.GetStats()

	// 返回统计信息
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取统计信息成功",
		"data":   stats,
	})
}

// HandleResetStats 重置统计信息
func HandleResetStats(c *gin.Context) {
	// 解析请求
	var req RuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    fmt.Sprintf("请求参数错误: %v", err),
		})
		return
	}

	// 验证参数
	if req.ArchiveName == "" || req.WorldName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称和世界名称",
		})
		return
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(req.ArchiveName, req.WorldName)

	// 重置统计信息
	err := ruleManager.ResetStats()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("重置统计信息失败: %v", err),
		})
		return
	}

	// 返回成功
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "重置统计信息成功",
		"data":   ruleManager.GetStats(),
	})
}

// HandleDeleteRule 删除规则
func HandleDeleteRule(c *gin.Context) {
	// 解析请求
	var req RuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    fmt.Sprintf("请求参数错误: %v", err),
		})
		return
	}

	// 验证参数
	if req.ArchiveName == "" || req.WorldName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称和世界名称",
		})
		return
	}

	if req.RuleID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "规则ID不能为空",
		})
		return
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(req.ArchiveName, req.WorldName)

	// 删除规则
	err := ruleManager.DeleteRule(req.RuleID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("删除规则失败: %v", err),
		})
		return
	}

	// 返回成功
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "删除规则成功",
	})
}

// HandleEnableRule 启用规则
func HandleEnableRule(c *gin.Context) {
	// 解析请求
	var req RuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    fmt.Sprintf("请求参数错误: %v", err),
		})
		return
	}

	// 验证参数
	if req.ArchiveName == "" || req.WorldName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称和世界名称",
		})
		return
	}

	if req.RuleID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "规则ID不能为空",
		})
		return
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(req.ArchiveName, req.WorldName)

	// 启用规则
	err := ruleManager.EnableRule(req.RuleID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("启用规则失败: %v", err),
		})
		return
	}

	// 返回成功
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "启用规则成功",
	})
}

// HandleDisableRule 禁用规则
func HandleDisableRule(c *gin.Context) {
	// 解析请求
	var req RuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    fmt.Sprintf("请求参数错误: %v", err),
		})
		return
	}

	// 验证参数
	if req.ArchiveName == "" || req.WorldName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "必须提供存档名称和世界名称",
		})
		return
	}

	if req.RuleID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "规则ID不能为空",
		})
		return
	}

	// 获取规则管理器
	ruleManager := GetOrCreateRuleManager(req.ArchiveName, req.WorldName)

	// 禁用规则
	err := ruleManager.DisableRule(req.RuleID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("禁用规则失败: %v", err),
		})
		return
	}

	// 返回成功
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "禁用规则成功",
	})
}
