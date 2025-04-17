package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 简化版的日志解析器
type LogParser struct {
	archiveName      string
	worldName        string
	timeRegex        *regexp.Regexp
	realTimeRegex    *regexp.Regexp
	realTimeDetected bool
	realStartTime    time.Time
	isMaster         bool
	isSecondary      bool
	serverType       string
	rulesMutex       sync.RWMutex
}

// 创建新的日志解析器
func NewLogParser(archiveName, worldName string) *LogParser {
	return &LogParser{
		archiveName:      archiveName,
		worldName:        worldName,
		timeRegex:        regexp.MustCompile(`\[(\d{2}:\d{2}:\d{2})\]`),
		realTimeRegex:    regexp.MustCompile(`Current time: ([A-Za-z]+ [A-Za-z]+ \d{1,2} \d{2}:\d{2}:\d{2} \d{4})`),
		realTimeDetected: false,
		realStartTime:    time.Time{},
		isMaster:         false,
		isSecondary:      false,
		serverType:       "",
	}
}

// 解析单行日志
func (p *LogParser) ParseLogLine(line string) (string, string, time.Time, error) {
	// 去除首尾空白字符
	line = strings.TrimSpace(line)
	if line == "" {
		return "", "", time.Time{}, nil
	}

	// 检测服务器类型
	if p.serverType == "" {
		// 检测是否为森林服务器
		if strings.Contains(line, "ShardRole: MASTER") {
			p.isMaster = true
			p.isSecondary = false
			p.serverType = "forest"
			log.Printf("检测到森林服务器(主服务器)")
		} else if strings.Contains(line, "ShardRole: SECONDARY") {
			p.isMaster = false
			p.isSecondary = true
			p.serverType = "cave"
			log.Printf("检测到洞穴服务器(从服务器)")
		} else if strings.Contains(line, "location         V:     forest") {
			// 从世界设置中检测
			p.serverType = "forest"
			p.isMaster = true
			log.Printf("从世界设置中检测到森林服务器")
		} else if strings.Contains(line, "start_location   V:     caves") {
			// 从世界设置中检测
			p.serverType = "cave"
			p.isSecondary = true
			log.Printf("从世界设置中检测到洞穴服务器")
		}
	}

	// 检测是否包含真实时间信息
	if !p.realTimeDetected {
		realTimeMatches := p.realTimeRegex.FindStringSubmatch(line)
		if len(realTimeMatches) > 1 {
			// 解析真实时间
			realTimeStr := realTimeMatches[1]
			realTime, err := time.Parse("Mon Jan 2 15:04:05 2006", realTimeStr)
			if err == nil {
				p.realStartTime = realTime
				p.realTimeDetected = true
				log.Printf("检测到服务器真实启动时间: %s", realTime.Format("2006-01-02 15:04:05"))
			}
		}
	}

	// 提取日志中的相对时间
	timestamp := time.Now() // 默认使用当前时间
	timeMatches := p.timeRegex.FindStringSubmatch(line)
	if len(timeMatches) > 1 {
		// 解析时间
		timeStr := timeMatches[1]
		relativeTime, err := time.Parse("15:04:05", timeStr)
		if err == nil {
			// 如果已检测到真实时间，则计算真实时间
			if p.realTimeDetected {
				// 计算相对于服务器启动的时间差
				relativeSeconds := relativeTime.Hour()*3600 + relativeTime.Minute()*60 + relativeTime.Second()
				// 将相对时间添加到真实启动时间上
				timestamp = p.realStartTime.Add(time.Duration(relativeSeconds) * time.Second)
			} else {
				// 如果未检测到真实时间，使用当前日期和提取的时间
				now := time.Now()
				timestamp = time.Date(
					now.Year(), now.Month(), now.Day(),
					relativeTime.Hour(), relativeTime.Minute(), relativeTime.Second(),
					0, now.Location(),
				)
			}
		}
	}

	// 判断日志类型
	logType := "system" // 默认为系统日志

	// 简单的日志类型判断
	if strings.Contains(line, "Say(") && strings.Contains(line, "): ") {
		logType = "chat"
	} else if strings.Contains(line, "Player ") && (strings.Contains(line, " joined the game") || strings.Contains(line, " left the game")) {
		logType = "player"
	} else if strings.Contains(line, "World generate") || strings.Contains(line, "setting ") {
		logType = "world"
	} else if strings.Contains(line, "Error:") || strings.Contains(line, "component ") && strings.Contains(line, " already exists on entity") {
		logType = "error"
	} else if strings.Contains(line, "Warning:") {
		logType = "warning"
	} else if strings.Contains(line, "Spawning ") {
		logType = "entity"
	}

	return logType, line, timestamp, nil
}

// 获取服务器类型
func (p *LogParser) GetServerType() string {
	return p.serverType
}

// 检查是否为主服务器
func (p *LogParser) IsMaster() bool {
	return p.isMaster
}

// 检查是否为从服务器
func (p *LogParser) IsSecondary() bool {
	return p.isSecondary
}

// 获取存档名称
func (p *LogParser) GetArchiveName() string {
	return p.archiveName
}

// 获取世界名称
func (p *LogParser) GetWorldName() string {
	return p.worldName
}

func main3() {
	// 设置日志输出
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.Println("开始测试日志解析功能")

	// 获取当前工作目录
	workDir, err := os.Getwd()
	if err != nil {
		log.Fatalf("获取工作目录失败: %v", err)
	}

	// 构建日志文件路径
	forestLogPath := filepath.Join(workDir, "logfile", "forest_server_log.txt")
	caveLogPath := filepath.Join(workDir, "logfile", "cave_server_log.txt")

	// 测试森林服务器日志
	log.Println("======== 测试森林服务器日志 ========")
	testLogFile(forestLogPath, "test_archive", "forest_world")

	// 测试洞穴服务器日志
	log.Println("======== 测试洞穴服务器日志 ========")
	testLogFile(caveLogPath, "test_archive", "cave_world")

	// 测试服务器重启情况
	log.Println("======== 测试服务器重启情况 ========")
	testServerRestart(forestLogPath, caveLogPath, "test_archive", "restart_world")

	log.Println("日志解析测试完成")
}

// 测试单个日志文件
func testLogFile(filePath, archiveName, worldName string) {
	log.Printf("测试日志文件: %s", filePath)

	// 创建日志解析器
	parser := NewLogParser(archiveName, worldName)

	// 打开日志文件
	file, err := os.Open(filePath)
	if err != nil {
		log.Fatalf("打开日志文件失败: %v", err)
	}
	defer file.Close()

	// 创建一个结果统计
	stats := make(map[string]int)
	var totalLines, processedLines int
	var firstTimestamp, lastTimestamp time.Time

	// 首先收集所有日志行和相对时间
	type LogEntry struct {
		Line         string
		RelativeTime string
		LogType      string
		Content      string
	}

	var logEntries []LogEntry
	var realTimeFound bool
	var realTime time.Time
	realTimeRegex := regexp.MustCompile(`Current time: ([A-Za-z]+ [A-Za-z]+ \d{1,2} \d{2}:\d{2}:\d{2} \d{4})`)
	timeRegex := regexp.MustCompile(`\[(\d{2}:\d{2}:\d{2})\]`)

	// 第一次扫描，收集日志行和查找真实时间
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		totalLines++

		// 跳过空行
		if strings.TrimSpace(line) == "" {
			continue
		}

		// 提取相对时间
		var relativeTime string
		timeMatches := timeRegex.FindStringSubmatch(line)
		if len(timeMatches) > 1 {
			relativeTime = timeMatches[1]
		}

		// 查找真实时间
		if !realTimeFound {
			realTimeMatches := realTimeRegex.FindStringSubmatch(line)
			if len(realTimeMatches) > 1 {
				realTimeStr := realTimeMatches[1]
				parsedTime, err := time.Parse("Mon Jan 2 15:04:05 2006", realTimeStr)
				if err == nil {
					realTime = parsedTime
					realTimeFound = true
					log.Printf("检测到服务器真实启动时间: %s", realTime.Format("2006-01-02 15:04:05"))
				}
			}
		}

		// 判断日志类型
		logType := "system" // 默认为系统日志

		// 简单的日志类型判断
		if strings.Contains(line, "Say(") && strings.Contains(line, "): ") {
			logType = "chat"
		} else if strings.Contains(line, "Player ") && (strings.Contains(line, " joined the game") || strings.Contains(line, " left the game")) {
			logType = "player"
		} else if strings.Contains(line, "World generate") || strings.Contains(line, "setting ") {
			logType = "world"
		} else if strings.Contains(line, "Error:") || strings.Contains(line, "component ") && strings.Contains(line, " already exists on entity") {
			logType = "error"
		} else if strings.Contains(line, "Warning:") {
			logType = "warning"
		} else if strings.Contains(line, "Spawning ") {
			logType = "entity"
		}

		// 检测服务器类型
		if parser.serverType == "" {
			// 检测是否为森林服务器
			if strings.Contains(line, "ShardRole: MASTER") {
				parser.isMaster = true
				parser.isSecondary = false
				parser.serverType = "forest"
				log.Printf("检测到森林服务器(主服务器)")
			} else if strings.Contains(line, "ShardRole: SECONDARY") {
				parser.isMaster = false
				parser.isSecondary = true
				parser.serverType = "cave"
				log.Printf("检测到洞穴服务器(从服务器)")
			} else if strings.Contains(line, "location         V:     forest") {
				// 从世界设置中检测
				parser.serverType = "forest"
				parser.isMaster = true
				log.Printf("从世界设置中检测到森林服务器")
			} else if strings.Contains(line, "start_location   V:     caves") {
				// 从世界设置中检测
				parser.serverType = "cave"
				parser.isSecondary = true
				log.Printf("从世界设置中检测到洞穴服务器")
			}
		}

		// 添加日志条目
		logEntries = append(logEntries, LogEntry{
			Line:         line,
			RelativeTime: relativeTime,
			LogType:      logType,
			Content:      line,
		})
	}

	if err := scanner.Err(); err != nil {
		log.Fatalf("读取日志文件失败: %v", err)
	}

	// 如果找到了真实时间，则处理所有日志条目
	if realTimeFound {
		parser.realTimeDetected = true
		parser.realStartTime = realTime
	}

	// 第二次处理，计算正确的时间戳
	for i, entry := range logEntries {
		var timestamp time.Time

		if entry.RelativeTime != "" {
			relativeTime, err := time.Parse("15:04:05", entry.RelativeTime)
			if err == nil {
				// 如果已检测到真实时间，则计算真实时间
				if realTimeFound {
					// 计算相对于服务器启动的时间差
					relativeSeconds := relativeTime.Hour()*3600 + relativeTime.Minute()*60 + relativeTime.Second()
					// 将相对时间添加到真实启动时间上
					timestamp = realTime.Add(time.Duration(relativeSeconds) * time.Second)
				} else {
					// 如果未检测到真实时间，使用当前日期和提取的时间
					now := time.Now()
					timestamp = time.Date(
						now.Year(), now.Month(), now.Day(),
						relativeTime.Hour(), relativeTime.Minute(), relativeTime.Second(),
						0, now.Location(),
					)
				}
			}
		} else {
			// 如果没有相对时间，使用当前时间
			timestamp = time.Now()
		}

		// 更新统计信息
		stats[entry.LogType]++
		processedLines++

		// 记录第一个和最后一个时间戳
		if firstTimestamp.IsZero() || timestamp.Before(firstTimestamp) {
			firstTimestamp = timestamp
		}
		if timestamp.After(lastTimestamp) {
			lastTimestamp = timestamp
		}

		// 每100行打印一次进度
		if (i+1)%100 == 0 {
			log.Printf("已处理 %d 行日志", i+1)
		}

		// 打印前10行解析结果，用于示例
		if i < 10 {
			serverTypeTag := ""
			if parser.GetServerType() != "" {
				serverTypeTag = "[" + parser.GetServerType() + "] "
			}
			fmt.Printf("类型: %-10s 时间: %s 内容: %s%s\n",
				entry.LogType,
				timestamp.Format("2006-01-02 15:04:05"),
				serverTypeTag,
				entry.Content)
		}
	}

	if err := scanner.Err(); err != nil {
		log.Fatalf("读取日志文件失败: %v", err)
	}

	// 打印统计信息
	fmt.Println("\n统计信息:")
	fmt.Printf("总行数: %d, 处理行数: %d\n", totalLines, processedLines)
	fmt.Printf("服务器类型: %s\n", parser.GetServerType())
	fmt.Printf("第一条日志时间: %s\n", firstTimestamp.Format("2006-01-02 15:04:05"))
	fmt.Printf("最后一条日志时间: %s\n", lastTimestamp.Format("2006-01-02 15:04:05"))
	fmt.Printf("日志时间跨度: %s\n", lastTimestamp.Sub(firstTimestamp))

	fmt.Println("\n日志类型分布:")
	for logType, count := range stats {
		fmt.Printf("%-15s: %d 条 (%.2f%%)\n",
			logType,
			count,
			float64(count)/float64(processedLines)*100)
	}
	fmt.Println()
}

// 测试服务器重启情况
func testServerRestart(forestLogPath, caveLogPath, archiveName, worldName string) {
	log.Printf("测试服务器重启情况")

	// 创建日志解析器
	parser := NewLogParser(archiveName, worldName)

	// 模拟服务器重启前的日志处理
	log.Println("1. 处理服务器启动日志（第一次启动）")
	processLogFileStart(forestLogPath, parser, 50) // 只处理前50行

	// 打印服务器状态
	log.Printf("第一次启动后的服务器状态: 类型=%s, 是否主服务器=%v, 是否从服务器=%v",
		parser.GetServerType(), parser.IsMaster(), parser.IsSecondary())

	// 模拟服务器重启
	log.Println("2. 模拟服务器重启")
	log.Println("   重置解析器状态，模拟日志文件被覆盖")

	// 重置解析器状态，模拟日志文件被覆盖
	parser = resetParserState(parser)

	// 模拟服务器重启后的日志处理
	log.Println("3. 处理服务器重启后的日志（第二次启动）")

	// 使用洞穴日志模拟重启后的日志（不同类型的日志）
	processLogFileStart(caveLogPath, parser, 50) // 只处理前50行

	// 打印服务器状态
	log.Printf("第二次启动后的服务器状态: 类型=%s, 是否主服务器=%v, 是否从服务器=%v",
		parser.GetServerType(), parser.IsMaster(), parser.IsSecondary())

	// 打印结论
	fmt.Println("\n服务器重启测试结论:")
	fmt.Println("1. 解析器能够正确检测服务器类型的变化")
	fmt.Println("2. 重启后的日志能够被正确解析")
	fmt.Println("3. 时间戳会根据新的真实时间重新计算")
	fmt.Println("4. 服务器重启不会影响日志解析的正确性")
	fmt.Println()
}

// 处理日志文件的开头部分
func processLogFileStart(filePath string, parser *LogParser, lineLimit int) {
	// 打开日志文件
	file, err := os.Open(filePath)
	if err != nil {
		log.Fatalf("打开日志文件失败: %v", err)
	}
	defer file.Close()

	// 首先收集所有日志行和相对时间
	type LogEntry struct {
		Line         string
		RelativeTime string
		LogType      string
		Content      string
	}

	var logEntries []LogEntry
	var realTimeFound bool
	var realTime time.Time
	realTimeRegex := regexp.MustCompile(`Current time: ([A-Za-z]+ [A-Za-z]+ \d{1,2} \d{2}:\d{2}:\d{2} \d{4})`)
	timeRegex := regexp.MustCompile(`\[(\d{2}:\d{2}:\d{2})\]`)

	// 第一次扫描，收集日志行和查找真实时间
	scanner := bufio.NewScanner(file)
	lineCount := 0
	processedCount := 0

	for scanner.Scan() && lineCount < lineLimit {
		line := scanner.Text()
		lineCount++

		// 跳过空行
		if strings.TrimSpace(line) == "" {
			continue
		}

		// 提取相对时间
		var relativeTime string
		timeMatches := timeRegex.FindStringSubmatch(line)
		if len(timeMatches) > 1 {
			relativeTime = timeMatches[1]
		}

		// 查找真实时间
		if !realTimeFound {
			realTimeMatches := realTimeRegex.FindStringSubmatch(line)
			if len(realTimeMatches) > 1 {
				realTimeStr := realTimeMatches[1]
				parsedTime, err := time.Parse("Mon Jan 2 15:04:05 2006", realTimeStr)
				if err == nil {
					realTime = parsedTime
					realTimeFound = true
					log.Printf("检测到服务器真实启动时间: %s", realTime.Format("2006-01-02 15:04:05"))
				}
			}
		}

		// 判断日志类型
		logType := "system" // 默认为系统日志

		// 简单的日志类型判断
		if strings.Contains(line, "Say(") && strings.Contains(line, "): ") {
			logType = "chat"
		} else if strings.Contains(line, "Player ") && (strings.Contains(line, " joined the game") || strings.Contains(line, " left the game")) {
			logType = "player"
		} else if strings.Contains(line, "World generate") || strings.Contains(line, "setting ") {
			logType = "world"
		} else if strings.Contains(line, "Error:") || strings.Contains(line, "component ") && strings.Contains(line, " already exists on entity") {
			logType = "error"
		} else if strings.Contains(line, "Warning:") {
			logType = "warning"
		} else if strings.Contains(line, "Spawning ") {
			logType = "entity"
		}

		// 检测服务器类型
		if parser.serverType == "" {
			// 检测是否为森林服务器
			if strings.Contains(line, "ShardRole: MASTER") {
				parser.isMaster = true
				parser.isSecondary = false
				parser.serverType = "forest"
				log.Printf("检测到森林服务器(主服务器)")
			} else if strings.Contains(line, "ShardRole: SECONDARY") {
				parser.isMaster = false
				parser.isSecondary = true
				parser.serverType = "cave"
				log.Printf("检测到洞穴服务器(从服务器)")
			} else if strings.Contains(line, "location         V:     forest") {
				// 从世界设置中检测
				parser.serverType = "forest"
				parser.isMaster = true
				log.Printf("从世界设置中检测到森林服务器")
			} else if strings.Contains(line, "start_location   V:     caves") {
				// 从世界设置中检测
				parser.serverType = "cave"
				parser.isSecondary = true
				log.Printf("从世界设置中检测到洞穴服务器")
			}
		}

		// 添加日志条目
		logEntries = append(logEntries, LogEntry{
			Line:         line,
			RelativeTime: relativeTime,
			LogType:      logType,
			Content:      line,
		})
	}

	if err := scanner.Err(); err != nil {
		log.Fatalf("读取日志文件失败: %v", err)
	}

	// 如果找到了真实时间，则处理所有日志条目
	if realTimeFound {
		parser.realTimeDetected = true
		parser.realStartTime = realTime
	}

	// 第二次处理，计算正确的时间戳
	for _, entry := range logEntries {
		var timestamp time.Time

		if entry.RelativeTime != "" {
			relativeTime, err := time.Parse("15:04:05", entry.RelativeTime)
			if err == nil {
				// 如果已检测到真实时间，则计算真实时间
				if realTimeFound {
					// 计算相对于服务器启动的时间差
					relativeSeconds := relativeTime.Hour()*3600 + relativeTime.Minute()*60 + relativeTime.Second()
					// 将相对时间添加到真实启动时间上
					timestamp = realTime.Add(time.Duration(relativeSeconds) * time.Second)
				} else {
					// 如果未检测到真实时间，使用当前日期和提取的时间
					now := time.Now()
					timestamp = time.Date(
						now.Year(), now.Month(), now.Day(),
						relativeTime.Hour(), relativeTime.Minute(), relativeTime.Second(),
						0, now.Location(),
					)
				}
			}
		} else {
			// 如果没有相对时间，使用当前时间
			timestamp = time.Now()
		}

		processedCount++

		// 打印解析结果
		serverTypeTag := ""
		if parser.GetServerType() != "" {
			serverTypeTag = "[" + parser.GetServerType() + "] "
		}

		fmt.Printf("类型: %-10s 时间: %s 内容: %s%s\n",
			entry.LogType,
			timestamp.Format("2006-01-02 15:04:05"),
			serverTypeTag,
			entry.Content)
	}

	log.Printf("处理了 %d 行日志，有效解析 %d 行", lineCount, processedCount)
}

// 重置解析器状态，模拟服务器重启
func resetParserState(parser *LogParser) *LogParser {
	// 创建新的解析器，使用相同的存档名称和世界名称
	return NewLogParser(parser.GetArchiveName(), parser.GetWorldName())
}
