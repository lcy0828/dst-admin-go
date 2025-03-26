package dstcustomize

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"regexp"
)

// 游戏自定义配置响应结构
type CustomizeResponse struct {
	Status int         `json:"status"`
	Data   interface{} `json:"data"`
}

// GetCustomizeLua 使用gopher-lua解析customize.lua文件并返回JSON格式数据
func GetCustomizeLua(g *gin.Context) {
	// 指定的customize.lua文件路径
	customizeLuaPath := "/dont/dont/misc/customize.lua"
	
	// 检查文件是否存在
	if _, err := os.Stat(customizeLuaPath); os.IsNotExist(err) {
		g.JSON(http.StatusOK, CustomizeResponse{
			Status: 404,
			Data:   "找不到customize.lua文件",
		})
		return
	}
	
	// 读取customize.lua文件内容
	content, err := ioutil.ReadFile(customizeLuaPath)
	if err != nil {
		log.Printf("读取customize.lua失败: %v", err)
		g.JSON(http.StatusOK, CustomizeResponse{
			Status: 500,
			Data:   fmt.Sprintf("读取文件失败: %v", err),
		})
		return
	}
	
	// 先尝试使用提取方法获取主要表结构
	extractedData := extractKeyTablesImproved(string(content))
	
	// 将提取的表格数据以结构化方式返回
	structuredData := processExtractedData(extractedData)
	
	// 返回解析结果
	g.JSON(http.StatusOK, CustomizeResponse{
		Status: 200,
		Data:   structuredData,
	})
}

// extractKeyTablesImproved 提取关键表结构
func extractKeyTablesImproved(content string) map[string]string {
	result := make(map[string]string)
	
	// 主要配置表名列表
	tableNames := []string{
		"WORLDGEN", "WORLDSETTINGS_GROUP", "WORLDGEN_GROUP",
		"frequency_descriptions", "worldgen_frequency_descriptions",
		"yesno_descriptions", "day_descriptions", "season_length_descriptions",
		"STRINGS", "MISC", "OPTIONS_MISC", "MOD_OPTIONS", 
	}
	
	for _, tableName := range tableNames {
		tableContent := extractLuaTableImproved(content, tableName)
		if tableContent != "" {
			result[tableName] = tableContent
		}
	}
	
	return result
}

// extractLuaTableImproved 从Lua内容中提取指定的表结构，改进版
func extractLuaTableImproved(content, tableName string) string {
	// 查找表的定义模式
	patterns := []string{
		tableName + "\\s*=\\s*\\{", // 全局变量
		"local\\s+" + tableName + "\\s*=\\s*\\{", // 局部变量
	}
	
	var tableStart int = -1
	
	// 尝试查找表的开始位置
	for _, patternStr := range patterns {
		re := regexp.MustCompile(patternStr)
		loc := re.FindStringIndex(content)
		if loc != nil {
			tableStart = loc[0]
			break
		}
	}
	
	if tableStart == -1 {
		return ""
	}
	
	// 找到表的括号范围
	bracketCount := 0
	inString := false
	inComment := false
	multilineComment := false
	
	// 查找匹配的第一个{
	openBracketIndex := strings.Index(content[tableStart:], "{") + tableStart
	if openBracketIndex == -1 {
		return ""
	}
	
	// 从{开始搜索匹配的}
	var tableEnd int = -1
	
	for i := openBracketIndex + 1; i < len(content); i++ {
		char := content[i]
		prevChar := byte(' ')
		if i > 0 {
			prevChar = content[i-1]
		}
		
		// 处理注释
		if !inString {
			// 处理行注释
			if char == '-' && prevChar == '-' && !inComment && !multilineComment {
				inComment = true
				continue
			}
			
			// 处理多行注释开始
			if i < len(content)-3 && content[i:i+4] == "--[[" && !inComment && !multilineComment {
				multilineComment = true
				i += 3 // 跳过[[
				continue
			}
			
			// 处理多行注释结束
			if i < len(content)-1 && content[i:i+2] == "]]" && multilineComment {
				multilineComment = false
				i += 1 // 跳过]
				continue
			}
			
			// 如果在行注释中，检查是否换行
			if inComment && char == '\n' {
				inComment = false
			}
			
			// 如果在注释中，跳过
			if inComment || multilineComment {
				continue
			}
		}
		
		// 处理字符串
		if (char == '"' || char == '\'') && (prevChar != '\\') {
			inString = !inString
			continue
		}
		
		// 处理括号，只在不在字符串和注释中时计数
		if !inString && !inComment && !multilineComment {
			if char == '{' {
				bracketCount++
			} else if char == '}' {
				bracketCount--
				// 找到匹配的最外层括号
				if bracketCount == -1 {
					tableEnd = i + 1
					break
				}
			}
		}
	}
	
	if tableEnd != -1 && tableEnd > tableStart {
		return content[tableStart:tableEnd]
	}
	
	return ""
}

// processExtractedData 处理提取的数据，使其更好地结构化
func processExtractedData(extractedData map[string]string) map[string]interface{} {
	result := make(map[string]interface{})
	
	// 处理每个提取的表格
	for tableName, tableContent := range extractedData {
		// 将表格内容解析为嵌套结构
		parsedTable := parseLuaTableContent(tableContent)
		result[tableName] = parsedTable
	}
	
	return result
}

// parseLuaTableContent 解析lua表内容为嵌套结构
func parseLuaTableContent(tableContent string) interface{} {
	// 找到表格的实际开始位置
	bracketStart := strings.Index(tableContent, "{")
	if bracketStart == -1 {
		return tableContent
	}
	
	// 提取表格内容（去掉表名和外层括号）
	cleanContent := tableContent[bracketStart+1:]
	// 去掉最后的括号
	lastBracket := strings.LastIndex(cleanContent, "}")
	if lastBracket != -1 {
		cleanContent = cleanContent[:lastBracket]
	}
	
	// 删除注释
	cleanContent = removeComments(cleanContent)
	
	// 解析为结构化数据
	result := parseTableStructure(cleanContent)
	return result
}

// removeComments 移除lua代码中的注释
func removeComments(content string) string {
	lines := strings.Split(content, "\n")
	result := make([]string, 0, len(lines))
	
	inMultilineComment := false
	
	for _, line := range lines {
		if inMultilineComment {
			// 寻找多行注释的结束
			endIndex := strings.Index(line, "]]")
			if endIndex != -1 {
				inMultilineComment = false
				line = line[endIndex+2:]
			} else {
				continue
			}
		}
		
		// 处理单行注释
		commentIndex := strings.Index(line, "--")
		if commentIndex != -1 {
			// 检查是否为多行注释开始
			if strings.HasPrefix(line[commentIndex:], "--[[") {
				line = line[:commentIndex]
				inMultilineComment = true
			} else {
				line = line[:commentIndex]
			}
		}
		
		// 添加非空行
		if strings.TrimSpace(line) != "" {
			result = append(result, line)
		}
	}
	
	return strings.Join(result, "\n")
}

// parseTableStructure 分析表结构
func parseTableStructure(content string) map[string]interface{} {
	result := make(map[string]interface{})
	
	// 定义递归解析嵌套表的函数
	var parseNested func(string) interface{}
	parseNested = func(text string) interface{} {
		// 检查是否是嵌套表
		if strings.HasPrefix(strings.TrimSpace(text), "{") && strings.HasSuffix(strings.TrimSpace(text), "}") {
			// 去掉括号
			innerText := strings.TrimSpace(text)
			innerText = innerText[1 : len(innerText)-1]
			
			// 区分数组和映射表
			if strings.Contains(innerText, "=") {
				// 是映射表
				return parseTableStructure(innerText)
			} else {
				// 是数组
				parts := splitByCommas(innerText)
				array := make([]interface{}, 0, len(parts))
				for _, part := range parts {
					part = strings.TrimSpace(part)
					if part != "" {
						array = append(array, parseValue(part))
					}
				}
				return array
			}
		}
		
		// 不是嵌套表，解析为基本值
		return parseValue(text)
	}
	
	// 分析所有键值对
	pairs := extractKeyValuePairs(content)
	for _, pair := range pairs {
		if len(pair) >= 2 {
			key := parseKey(pair[0])
			value := parseNested(pair[1])
			result[key] = value
		}
	}
	
	return result
}

// extractKeyValuePairs 提取键值对
func extractKeyValuePairs(content string) [][]string {
	var pairs [][]string
	
	// 使用状态机来解析
	inString := false
	inKey := true
	bracketCount := 0
	escaped := false
	
	var currentKey, currentValue strings.Builder
	
	for i := 0; i < len(content); i++ {
		char := content[i]
		
		// 处理字符串
		if (char == '"' || char == '\'') && !escaped {
			inString = !inString
		}
		
		// 处理转义字符
		if char == '\\' && !escaped {
			escaped = true
			continue
		} else {
			escaped = false
		}
		
		// 处理括号
		if !inString {
			if char == '{' {
				bracketCount++
			} else if char == '}' {
				bracketCount--
			}
		}
		
		// 处理键值分隔符
		if char == '=' && inKey && bracketCount == 0 && !inString {
			inKey = false
			continue
		}
		
		// 处理逗号和分号作为项目分隔符
		if (char == ',' || char == ';') && !inString && bracketCount == 0 {
			// 完成一个键值对
			key := strings.TrimSpace(currentKey.String())
			value := strings.TrimSpace(currentValue.String())
			
			if key != "" {
				pairs = append(pairs, []string{key, value})
			}
			
			// 重置
			currentKey.Reset()
			currentValue.Reset()
			inKey = true
			continue
		}
		
		// 收集字符
		if inKey {
			currentKey.WriteByte(char)
		} else {
			currentValue.WriteByte(char)
		}
	}
	
	// 处理最后一个键值对
	key := strings.TrimSpace(currentKey.String())
	value := strings.TrimSpace(currentValue.String())
	if key != "" {
		pairs = append(pairs, []string{key, value})
	}
	
	return pairs
}

// parseKey 解析键
func parseKey(key string) string {
	key = strings.TrimSpace(key)
	
	// 如果键被方括号包围，去掉方括号和引号
	if strings.HasPrefix(key, "[") && strings.HasSuffix(key, "]") {
		key = key[1 : len(key)-1]
		key = strings.Trim(key, "\"'")
	}
	
	return key
}

// parseValue 解析值
func parseValue(value string) interface{} {
	value = strings.TrimSpace(value)
	
	// 处理nil
	if value == "nil" {
		return nil
	}
	
	// 处理布尔值
	if value == "true" {
		return true
	}
	if value == "false" {
		return false
	}
	
	// 处理字符串（带引号）
	if (strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"")) ||
		(strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'")) {
		return value[1 : len(value)-1]
	}
	
	// 尝试解析为数字
	// 这里简化处理，实际上应该用更精确的数字解析
	return value
}

// splitByCommas 按逗号分割字符串，考虑嵌套结构
func splitByCommas(text string) []string {
	var result []string
	
	bracketCount := 0
	inString := false
	escaped := false
	var current strings.Builder
	
	for _, char := range text {
		// 处理转义
		if char == '\\' && !escaped {
			escaped = true
			current.WriteRune(char)
			continue
		}
		
		// 处理字符串
		if (char == '"' || char == '\'') && !escaped {
			inString = !inString
		}
		
		// 重置转义状态
		if escaped {
			escaped = false
		}
		
		// 处理括号
		if !inString {
			if char == '{' {
				bracketCount++
			} else if char == '}' {
				bracketCount--
			}
		}
		
		// 处理逗号分隔符
		if char == ',' && bracketCount == 0 && !inString {
			result = append(result, current.String())
			current.Reset()
			continue
		}
		
		// 收集字符
		current.WriteRune(char)
	}
	
	// 添加最后一部分
	if current.Len() > 0 {
		result = append(result, current.String())
	}
	
	return result
} 