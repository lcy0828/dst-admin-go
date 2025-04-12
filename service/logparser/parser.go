package logparser

import (
	"dont/models"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
)

// LogEntry 日志条目，用于批量处理
type LogEntry struct {
	Line         string    // 原始日志行
	RelativeTime string    // 相对时间
	LogType      string    // 日志类型
	Content      string    // 日志内容
	Timestamp    time.Time // 时间戳
}

// LogParser 日志解析器
type LogParser struct {
	rules        []models.LogExtractRule // 提取规则
	rulesMutex   sync.RWMutex            // 规则读写锁
	archiveName  string                  // 存档名称
	worldName    string                  // 世界名称
	timeRegex    *regexp.Regexp          // 时间提取正则表达式
	defaultRules []models.LogExtractRule // 默认规则

	// 时间校正相关字段
	realStartTime    time.Time      // 服务器实际启动时间
	realTimeDetected bool           // 是否已检测到实际时间
	realTimeRegex    *regexp.Regexp // 实际时间提取正则表达式

	// 服务器类型相关字段
	isMaster    bool   // 是否为主服务器（森林服务器）
	isSecondary bool   // 是否为从服务器（洞穴服务器）
	serverType  string // 服务器类型（"forest"或"cave"）

	// 日志缓冲相关字段
	logBuffer     []models.GameLog // 日志缓冲区
	bufferMutex   sync.Mutex       // 缓冲区互斥锁
	bufferSize    int              // 缓冲区大小阈值，超过此值将触发批量写入
	lastFlushTime time.Time        // 上次刷新缓冲区的时间
}

// NewLogParser 创建新的日志解析器
func NewLogParser(archiveName, worldName string) (*LogParser, error) {
	// 初始化日志解析器
	parser := &LogParser{
		archiveName:      archiveName,
		worldName:        worldName,
		timeRegex:        regexp.MustCompile(`\[(\d{2}:\d{2}:\d{2})\]`),                                             // 匹配[HH:MM:SS]格式的时间
		realTimeRegex:    regexp.MustCompile(`Current time: ([A-Za-z]+ [A-Za-z]+ \d{1,2} \d{2}:\d{2}:\d{2} \d{4})`), // 匹配真实时间
		realTimeDetected: false,
		realStartTime:    time.Time{}, // 初始化为零值

		// 初始化服务器类型相关字段
		isMaster:    false,
		isSecondary: false,
		serverType:  "", // 将在解析日志时自动检测

		// 初始化日志缓冲相关字段
		logBuffer:     make([]models.GameLog, 0, 100), // 初始容量为100
		bufferSize:    50,                             // 默认缓冲区大小阈值为50
		lastFlushTime: time.Now(),                     // 初始化为当前时间
	}

	// 加载默认规则
	parser.initDefaultRules()

	// 从数据库加载规则
	if err := parser.LoadRules(); err != nil {
		return nil, err
	}

	return parser, nil
}

// 初始化默认规则
func (p *LogParser) initDefaultRules() {
	// 初始化通用规则
	commonRules := []models.LogExtractRule{
		// 服务器启动相关
		{
			Name:        "服务器启动",
			Description: "匹配服务器启动信息",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: Starting Up`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    100,
		},
		{
			Name:        "服务器版本",
			Description: "匹配服务器版本信息",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: Version: \d+`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    95,
		},
		{
			Name:        "服务器时间",
			Description: "匹配服务器当前时间",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: Current time:`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    95,
		},

		// 玩家相关
		{
			Name:        "聊天消息",
			Description: "匹配玩家聊天消息",
			LogType:     models.LogTypeChat,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\].*?Say\(.*?\): .*`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    100,
		},
		{
			Name:        "玩家加入",
			Description: "匹配玩家加入服务器",
			LogType:     models.LogTypePlayer,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\].*?Player (.*?) joined the game`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    90,
		},
		{
			Name:        "玩家离开",
			Description: "匹配玩家离开服务器",
			LogType:     models.LogTypePlayer,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\].*?Player (.*?) left the game`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    90,
		},

		// 世界相关
		{
			Name:        "世界生成",
			Description: "匹配世界生成事件",
			LogType:     models.LogTypeWorld,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\].*?World generate`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    80,
		},
		{
			Name:        "世界设置",
			Description: "匹配世界设置信息",
			LogType:     models.LogTypeWorld,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: setting\s+\w+\s+.*`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    80,
		},
		{
			Name:        "世界设置覆盖",
			Description: "匹配世界设置覆盖信息",
			LogType:     models.LogTypeWorld,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: OVERRIDE: setting\s+\w+\s+to\s+.*`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    80,
		},

		// 分片相关
		{
			Name:        "分片信息",
			Description: "匹配分片相关信息",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: \[Shard\].*`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    85,
		},
		{
			Name:        "分片连接",
			Description: "匹配分片连接信息",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: \[Shard\] Secondary .* connected:.*`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    85,
		},
		{
			Name:        "世界设置同步",
			Description: "匹配世界设置同步信息",
			LogType:     models.LogTypeWorld,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: \[SyncWorldSettings\].*`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    80,
		},

		// 传送门相关
		{
			Name:        "传送门验证",
			Description: "匹配传送门验证信息",
			LogType:     models.LogTypeEntity,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: Validating portal.*`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    75,
		},

		// 错误和警告
		{
			Name:        "错误日志",
			Description: "匹配错误日志",
			LogType:     models.LogTypeError,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\].*?Error:`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    70,
		},
		{
			Name:        "组件已存在错误",
			Description: "匹配组件已存在错误",
			LogType:     models.LogTypeError,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: component .* already exists on entity.*`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    70,
		},
		{
			Name:        "警告日志",
			Description: "匹配警告日志",
			LogType:     models.LogTypeWarning,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\].*?Warning:`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    60,
		},

		// 实体相关
		{
			Name:        "实体生成",
			Description: "匹配实体生成事件",
			LogType:     models.LogTypeEntity,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\].*?Spawning .*`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    50,
		},

		// 服务器网络相关
		{
			Name:        "服务器端口",
			Description: "匹配服务器端口信息",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: Online Server Started on port: \d+`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    85,
		},
		{
			Name:        "Steam初始化",
			Description: "匹配Steam初始化信息",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: \[Steam\].*`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    85,
		},
		{
			Name:        "服务器注册",
			Description: "匹配服务器注册信息",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: Server registered via geo DNS in .*`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    85,
		},

		// 模组相关
		{
			Name:        "模组加载",
			Description: "匹配模组加载信息",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: SUCCESS: Loaded modoverrides.lua`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    85,
		},
		{
			Name:        "模组索引",
			Description: "匹配模组索引信息",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: ModIndex:.*`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    85,
		},

		// 默认规则（最低优先级）
		{
			Name:        "系统消息",
			Description: "匹配系统消息",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\].*`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    10, // 最低优先级，作为默认匹配
		},
	}

	// 森林服务器（主服务器）特定规则
	forestRules := []models.LogExtractRule{
		{
			Name:        "主服务器启动",
			Description: "匹配主服务器启动信息",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: \[Shard\] Starting master server`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    95,
		},
		{
			Name:        "主服务器角色",
			Description: "匹配主服务器角色信息",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]:   ShardRole: MASTER`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    95,
		},
		{
			Name:        "从服务器连接",
			Description: "匹配从服务器连接信息",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: \[Shard\] Secondary shard .* connected:`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    95,
		},
		{
			Name:        "从服务器就绪",
			Description: "匹配从服务器就绪信息",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: \[Shard\] Secondary .* ready!`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    95,
		},
		{
			Name:        "发送世界设置",
			Description: "匹配发送世界设置信息",
			LogType:     models.LogTypeWorld,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: \[SyncWorldSettings\] Sending master world option .* to secondary shards.`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    90,
		},
		{
			Name:        "重新同步世界设置",
			Description: "匹配重新同步世界设置信息",
			LogType:     models.LogTypeWorld,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: \[SyncWorldSettings\] Resyncing master world option .* to secondary shards.`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    90,
		},
	}

	// 洞穴服务器（从服务器）特定规则
	caveRules := []models.LogExtractRule{
		{
			Name:        "从服务器角色",
			Description: "匹配从服务器角色信息",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]:   ShardRole: SECONDARY`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    95,
		},
		{
			Name:        "连接主服务器",
			Description: "匹配连接主服务器信息",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: \[Shard\] Connecting to master...`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    95,
		},
		{
			Name:        "发送从服务器信息",
			Description: "匹配发送从服务器信息",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: \[Shard\] Sending secondary shard information to master...`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    95,
		},
		{
			Name:        "从服务器就绪",
			Description: "匹配从服务器就绪信息",
			LogType:     models.LogTypeSystem,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: \[Shard\] secondary shard is now ready!`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    95,
		},
		{
			Name:        "接收世界设置",
			Description: "匹配接收世界设置信息",
			LogType:     models.LogTypeWorld,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: \[SyncWorldSettings\] recieved world settings from master shard.`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    90,
		},
		{
			Name:        "应用世界设置",
			Description: "匹配应用世界设置信息",
			LogType:     models.LogTypeWorld,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: \[SyncWorldSettings\] applying .* from master shard.`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    90,
		},
		{
			Name:        "组件已存在错误",
			Description: "匹配组件已存在错误",
			LogType:     models.LogTypeError,
			Pattern:     `\[\d{2}:\d{2}:\d{2}\]: component hauntable already exists on entity .* - multiplayer_portal!`,
			IsRegex:     true,
			IsEnabled:   true,
			Priority:    80,
		},
	}

	// 合并所有规则
	p.defaultRules = append(commonRules, forestRules...)
	p.defaultRules = append(p.defaultRules, caveRules...)
}

// LoadRules 从数据库加载规则
func (p *LogParser) LoadRules() error {
	p.rulesMutex.Lock()
	defer p.rulesMutex.Unlock()

	// 从数据库获取启用的规则
	rules, err := models.GetEnabledLogExtractRules()
	if err != nil {
		return err
	}

	// 如果数据库中没有规则，使用默认规则
	if len(rules) == 0 {
		p.rules = p.defaultRules
		// 将默认规则保存到数据库
		for _, rule := range p.defaultRules {
			models.AddLogExtractRule(
				rule.Name,
				rule.Description,
				rule.LogType,
				rule.Pattern,
				rule.IsRegex,
				rule.IsEnabled,
				rule.Priority,
			)
		}
	} else {
		p.rules = rules
	}

	return nil
}

// ReloadRules 重新加载规则
func (p *LogParser) ReloadRules() error {
	return p.LoadRules()
}

// GetServerType 获取服务器类型
func (p *LogParser) GetServerType() string {
	return p.serverType
}

// IsMaster 检查是否为主服务器
func (p *LogParser) IsMaster() bool {
	return p.isMaster
}

// IsSecondary 检查是否为从服务器
func (p *LogParser) IsSecondary() bool {
	return p.isSecondary
}

// GetArchiveName 获取存档名称
func (p *LogParser) GetArchiveName() string {
	return p.archiveName
}

// GetWorldName 获取世界名称
func (p *LogParser) GetWorldName() string {
	return p.worldName
}

// GetRealStartTime 获取服务器实际启动时间
func (p *LogParser) GetRealStartTime() time.Time {
	return p.realStartTime
}

// ParseLogLine 解析单行日志
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
	// 确保时区信息正确（东八区）
	cst := time.FixedZone("CST", 8*3600)
	// 默认使用当前时间，但不进行时区转换，只确保时区信息正确
	timestamp := time.Now()
	if timestamp.Location().String() == "UTC" {
		timestamp = time.Date(
			timestamp.Year(), timestamp.Month(), timestamp.Day(),
			timestamp.Hour(), timestamp.Minute(), timestamp.Second(),
			timestamp.Nanosecond(), cst,
		)
	}
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
				// 确保时区信息正确
				if timestamp.Location().String() == "UTC" {
					timestamp = time.Date(
						timestamp.Year(), timestamp.Month(), timestamp.Day(),
						timestamp.Hour(), timestamp.Minute(), timestamp.Second(),
						timestamp.Nanosecond(), cst,
					)
				}
			} else {
				// 如果未检测到真实时间，使用当前日期和提取的时间
				// 注意：这里我们使用当前时间，但在检测到真实时间后应该重新计算
				// 这个问题将在ProcessAndSaveLog函数中解决
				now := time.Now()
				// 确保时区信息正确
				if now.Location().String() == "UTC" {
					timestamp = time.Date(
						now.Year(), now.Month(), now.Day(),
						relativeTime.Hour(), relativeTime.Minute(), relativeTime.Second(),
						0, cst,
					)
				} else {
					timestamp = time.Date(
						now.Year(), now.Month(), now.Day(),
						relativeTime.Hour(), relativeTime.Minute(), relativeTime.Second(),
						0, now.Location(),
					)
				}
			}
		}
	}

	// 应用规则提取日志类型和内容
	p.rulesMutex.RLock()
	defer p.rulesMutex.RUnlock()

	for _, rule := range p.rules {
		var matched bool
		var content string

		if rule.IsRegex {
			// 使用正则表达式匹配
			re, err := regexp.Compile(rule.Pattern)
			if err != nil {
				continue // 跳过无效的正则表达式
			}

			if re.MatchString(line) {
				matched = true
				// 提取内容（可以根据需要自定义提取逻辑）
				content = line
			}
		} else {
			// 使用简单字符串匹配
			if strings.Contains(line, rule.Pattern) {
				matched = true
				content = line
			}
		}

		if matched {
			return rule.LogType, content, timestamp, nil
		}
	}

	// 如果没有匹配的规则，返回未知类型
	return models.LogTypeUnknown, line, timestamp, nil
}

// ProcessLogContent 处理日志内容
func (p *LogParser) ProcessLogContent(content string) []string {
	// 打印简化的调试信息
	log.Printf("[LogParser] 开始处理日志内容，长度: %d 字节", len(content))

	// 按行分割日志内容
	lines := strings.Split(content, "\n")

	result := make([]string, 0, len(lines))

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			// 检测是否为有效的日志行
			if p.timeRegex.MatchString(line) {
				result = append(result, line)
			} else {
				// 尝试其他日志格式
				// 有些日志可能没有时间戳，但仍然是有效的
				if strings.Contains(line, "[Connect]") ||
					strings.Contains(line, "[Disconnect]") ||
					strings.Contains(line, "[Join") ||
					strings.Contains(line, "[Leave") ||
					strings.Contains(line, "[Death") ||
					strings.Contains(line, "[Chat") {
					result = append(result, line)
				} else {
					// 尝试其他可能的日志格式
					if strings.Contains(line, ":") || strings.Contains(line, "[") {
						result = append(result, line)
					}
				}
			}
		}
	}

	log.Printf("[LogParser] 处理完成，共有 %d 行有效日志", len(result))
	return result
}

// SaveLogToDatabase 将日志保存到数据库
func (p *LogParser) SaveLogToDatabase(logType, content string, timestamp time.Time) error {
	// 使用原始存档名称，不添加服务器类型
	archiveName := p.archiveName

	// 分离原始内容和处理后的内容
	rawContent := content
	if strings.HasPrefix(content, "[") && strings.Contains(content, "] ") {
		// 如果内容已经包含服务器类型标记，提取原始内容
		parts := strings.SplitN(content, "] ", 2)
		if len(parts) > 1 {
			rawContent = parts[1]
		}
	}

	// 确保时间戳有正确的时区信息（东八区）
	if timestamp.Location().String() == "UTC" {
		cst := time.FixedZone("CST", 8*3600)
		timestamp = time.Date(
			timestamp.Year(), timestamp.Month(), timestamp.Day(),
			timestamp.Hour(), timestamp.Minute(), timestamp.Second(),
			timestamp.Nanosecond(), cst,
		)
	}

	// 创建日志记录
	log := models.GameLog{
		ArchiveName: archiveName,
		WorldName:   p.worldName,
		LogType:     logType,
		Content:     content,
		RawContent:  rawContent,
		Timestamp:   timestamp,
		CreatedAt:   time.Now(),
	}

	// 将日志添加到缓冲区
	return p.addToBuffer(log)
}

// addToBuffer 将日志添加到缓冲区
func (p *LogParser) addToBuffer(log models.GameLog) error {
	p.bufferMutex.Lock()
	defer p.bufferMutex.Unlock()

	// 添加日志到缓冲区
	p.logBuffer = append(p.logBuffer, log)

	// 检查是否需要刷新缓冲区
	if len(p.logBuffer) >= p.bufferSize || time.Since(p.lastFlushTime) > 5*time.Second {
		return p.flushBuffer()
	}

	return nil
}

// flushBuffer 刷新缓冲区，将日志批量写入数据库
func (p *LogParser) flushBuffer() error {
	// 如果缓冲区为空，直接返回
	if len(p.logBuffer) == 0 {
		return nil
	}

	// 复制缓冲区中的日志
	logs := make([]models.GameLog, len(p.logBuffer))
	copy(logs, p.logBuffer)

	// 清空缓冲区
	p.logBuffer = p.logBuffer[:0]

	// 更新最后刷新时间
	p.lastFlushTime = time.Now()

	// 释放锁后批量写入数据库
	p.bufferMutex.Unlock()
	err := models.AddGameLogBatch(logs)
	p.bufferMutex.Lock()

	if err != nil {
		log.Printf("[LogParser] 批量保存日志到数据库失败: %v", err)
	} else {
		log.Printf("[LogParser] 批量保存日志到数据库成功: %d 条记录", len(logs))
	}

	return err
}

// SaveLogToDatabaseBatch 批量将日志保存到数据库
func (p *LogParser) SaveLogToDatabaseBatch(entries []LogEntry) error {
	if len(entries) == 0 {
		return nil
	}

	log.Printf("[LogParser] 批量保存 %d 条日志到数据库", len(entries))

	// 准备批量插入的日志记录
	logs := make([]models.GameLog, 0, len(entries))
	cst := time.FixedZone("CST", 8*3600)

	for _, entry := range entries {
		// 分离原始内容和处理后的内容
		rawContent := entry.Content
		if strings.HasPrefix(entry.Content, "[") && strings.Contains(entry.Content, "] ") {
			// 如果内容已经包含服务器类型标记，提取原始内容
			parts := strings.SplitN(entry.Content, "] ", 2)
			if len(parts) > 1 {
				rawContent = parts[1]
			}
		}

		// 确保时间戳有正确的时区信息
		timestamp := entry.Timestamp
		if timestamp.Location().String() == "UTC" {
			timestamp = time.Date(
				timestamp.Year(), timestamp.Month(), timestamp.Day(),
				timestamp.Hour(), timestamp.Minute(), timestamp.Second(),
				timestamp.Nanosecond(), cst,
			)
		}

		// 创建日志记录
		log := models.GameLog{
			ArchiveName: p.archiveName,
			WorldName:   p.worldName,
			LogType:     entry.LogType,
			Content:     entry.Content,
			RawContent:  rawContent,
			Timestamp:   timestamp,
			CreatedAt:   time.Now(),
		}

		logs = append(logs, log)
	}

	// 批量插入日志记录
	err := models.AddGameLogBatch(logs)
	if err != nil {
		log.Printf("[LogParser] 批量保存日志到数据库失败: %v", err)
		return err
	}

	log.Printf("[LogParser] 批量保存日志到数据库成功")
	return nil
}

// ProcessAndSaveLog 处理并保存日志
func (p *LogParser) ProcessAndSaveLog(content string) error {
	log.Printf("[LogParser] 开始处理日志内容，存档=%s, 世界=%s, 内容长度=%d字节",
		p.archiveName, p.worldName, len(content))

	// 检查内容是否为空
	if len(content) == 0 {
		log.Printf("[LogParser] 内容为空，跳过处理")
		return nil
	}

	// 尝试处理日志内容
	lines := p.ProcessLogContent(content)
	log.Printf("[LogParser] 处理后得到 %d 行有效日志内容", len(lines))

	// 检查是否有有效的日志行
	if len(lines) == 0 {
		log.Printf("[LogParser] 没有有效的日志行，跳过处理")
		return nil
	}

	// 首先收集所有日志行和相对时间
	type LogEntry struct {
		Line         string
		RelativeTime string
		LogType      string
		Content      string
		Timestamp    time.Time
	}

	var logEntries []LogEntry
	var realTimeFound bool
	var realTime time.Time

	// 第一次扫描，收集日志行和查找真实时间
	for _, line := range lines {
		// 解析日志行
		logType, parsedContent, timestamp, err := p.ParseLogLine(line)
		if err != nil {
			continue // 跳过解析错误的行
		}

		// 如果解析结果为空，跳过
		if logType == "" || parsedContent == "" {
			continue
		}

		// 提取相对时间
		var relativeTime string
		timeMatches := p.timeRegex.FindStringSubmatch(line)
		if len(timeMatches) > 1 {
			relativeTime = timeMatches[1]
		}

		// 检查是否找到了真实时间
		if p.realTimeDetected && !realTimeFound {
			realTimeFound = true
			realTime = p.realStartTime
		}

		// 添加日志条目
		logEntries = append(logEntries, LogEntry{
			Line:         line,
			RelativeTime: relativeTime,
			LogType:      logType,
			Content:      parsedContent,
			Timestamp:    timestamp,
		})
	}

	// 如果找到了真实时间，则重新计算所有日志的时间戳
	// 确保时区信息正确（东八区）
	cst := time.FixedZone("CST", 8*3600)
	if realTimeFound {
		for _, entry := range logEntries {
			var timestamp time.Time

			if entry.RelativeTime != "" {
				relativeTime, err := time.Parse("15:04:05", entry.RelativeTime)
				if err == nil {
					// 计算相对于服务器启动的时间差
					relativeSeconds := relativeTime.Hour()*3600 + relativeTime.Minute()*60 + relativeTime.Second()
					// 将相对时间添加到真实启动时间上
					timestamp = realTime.Add(time.Duration(relativeSeconds) * time.Second)
					// 确保时区信息正确
					if timestamp.Location().String() == "UTC" {
						timestamp = time.Date(
							timestamp.Year(), timestamp.Month(), timestamp.Day(),
							timestamp.Hour(), timestamp.Minute(), timestamp.Second(),
							timestamp.Nanosecond(), cst,
						)
					}
				} else {
					// 如果解析时间失败，使用原始时间戳
					timestamp = entry.Timestamp
					// 确保时区信息正确
					if timestamp.Location().String() == "UTC" {
						timestamp = time.Date(
							timestamp.Year(), timestamp.Month(), timestamp.Day(),
							timestamp.Hour(), timestamp.Minute(), timestamp.Second(),
							timestamp.Nanosecond(), cst,
						)
					}
				}
			} else {
				// 如果没有相对时间，使用原始时间戳
				timestamp = entry.Timestamp
				// 确保时区信息正确
				if timestamp.Location().String() == "UTC" {
					timestamp = time.Date(
						timestamp.Year(), timestamp.Month(), timestamp.Day(),
						timestamp.Hour(), timestamp.Minute(), timestamp.Second(),
						timestamp.Nanosecond(), cst,
					)
				}
			}

			// 根据服务器类型添加标记
			var serverTypeTag string
			if p.serverType != "" {
				serverTypeTag = "[" + p.serverType + "] "
			} else if p.isMaster {
				serverTypeTag = "[forest] "
				p.serverType = "forest"
			} else if p.isSecondary {
				serverTypeTag = "[cave] "
				p.serverType = "cave"
			} else {
				// 尝试从世界名称推断服务器类型
				if strings.Contains(strings.ToLower(p.worldName), "cave") {
					serverTypeTag = "[cave] "
					p.serverType = "cave"
					p.isSecondary = true
				} else {
					// 默认为森林服务器
					serverTypeTag = "[forest] "
					p.serverType = "forest"
					p.isMaster = true
				}
			}

			// 添加服务器类型标记到内容中
			enhancedContent := serverTypeTag + entry.Content

			// 保存到数据库
			if err := p.SaveLogToDatabase(entry.LogType, enhancedContent, timestamp); err != nil {
				log.Printf("[LogParser] 保存日志到数据库失败: %v", err)
				return err
			}
		}
	} else {
		// 如果没有找到真实时间，则使用原始时间戳
		for _, entry := range logEntries {
			// 确保时区信息正确
			timestamp := entry.Timestamp
			if timestamp.Location().String() == "UTC" {
				timestamp = time.Date(
					timestamp.Year(), timestamp.Month(), timestamp.Day(),
					timestamp.Hour(), timestamp.Minute(), timestamp.Second(),
					timestamp.Nanosecond(), cst,
				)
			}
			// 根据服务器类型添加标记
			var serverTypeTag string
			if p.serverType != "" {
				serverTypeTag = "[" + p.serverType + "] "
			} else if p.isMaster {
				serverTypeTag = "[forest] "
				p.serverType = "forest"
			} else if p.isSecondary {
				serverTypeTag = "[cave] "
				p.serverType = "cave"
			} else {
				// 尝试从世界名称推断服务器类型
				if strings.Contains(strings.ToLower(p.worldName), "cave") {
					serverTypeTag = "[cave] "
					p.serverType = "cave"
					p.isSecondary = true
				} else {
					// 默认为森林服务器
					serverTypeTag = "[forest] "
					p.serverType = "forest"
					p.isMaster = true
				}
			}

			// 添加服务器类型标记到内容中
			enhancedContent := serverTypeTag + entry.Content

			// 保存到数据库
			if err := p.SaveLogToDatabase(entry.LogType, enhancedContent, timestamp); err != nil {
				log.Printf("[LogParser] 保存日志到数据库失败: %v", err)
				return err
			}
		}
	}

	return nil
}
