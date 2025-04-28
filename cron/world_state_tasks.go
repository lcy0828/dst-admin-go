package cron

import (
	"dont/models"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ReadWorldStateFile 从存档位置读取世界状态文件
// 参数:
// - archiveName: 存档名称
// - worldName: 世界名称
func ReadWorldStateFile(archiveName, worldName string) (string, error) {
	// 获取存档路径
	dstSavePath := getDstSavePath()
	if dstSavePath == "" {
		return "", fmt.Errorf("无法获取DST存档路径")
	}

	// 构建世界状态文件路径
	worldStatePath := filepath.Join(dstSavePath, archiveName, worldName, "save", "mod_config_data", "world_state")

	// 检查文件是否存在
	fileInfo, err := os.Stat(worldStatePath)
	if os.IsNotExist(err) {
		return "", fmt.Errorf("世界状态文件不存在: %s", worldStatePath)
	} else if err != nil {
		return "", fmt.Errorf("获取文件信息失败: %v", err)
	}

	// 获取文件修改时间和大小
	modTime := fileInfo.ModTime()
	fileSize := fileInfo.Size()

	// 检查文件是否被修改
	fileCacheMutex.RLock()
	cache, exists := fileCacheMap[worldStatePath]
	fileCacheMutex.RUnlock()

	if exists {
		// 如果文件大小和修改时间都没变，直接返回缓存的结果
		if cache.FileSize == fileSize && cache.ModTime.Equal(modTime) {
			// 构建返回消息，使用缓存的结果
			message := fmt.Sprintf("文件未变化，使用缓存结果。存档: %s, 世界: %s",
				archiveName, worldName)
			return message, nil
		}

		// 如果只有修改时间变了但大小没变，计算MD5确认内容是否真的变了
		if cache.FileSize == fileSize && !cache.ModTime.Equal(modTime) {
			// 计算文件MD5
			md5Str, err := calculateFileMD5(worldStatePath)
			if err == nil && md5Str == cache.ContentMD5 {
				// 内容没变，只更新缓存中的修改时间
				fileCacheMutex.Lock()
				cache.ModTime = modTime
				fileCacheMap[worldStatePath] = cache
				fileCacheMutex.Unlock()

				// 构建返回消息，使用缓存的结果
				message := fmt.Sprintf("文件内容未变化，使用缓存结果。存档: %s, 世界: %s",
					archiveName, worldName)
				return message, nil
			}
		}
	}

	// 只在文件有变化时记录日志
	log.Printf("[WorldStateTask] 读取世界状态文件，存档: %s, 世界: %s", archiveName, worldName)

	// 读取文件内容
	fileContent, err := os.ReadFile(worldStatePath)
	if err != nil {
		return "", fmt.Errorf("读取世界状态文件失败: %v", err)
	}

	// 计算文件MD5
	md5Str, err := calculateFileMD5(worldStatePath)
	if err != nil {
		// 减少不必要的日志输出，只在调试时输出
		// log.Printf("[WorldStateTask] 计算文件MD5失败: %v", err)
		// 继续处理，不中断流程
	}

	// 解析文件内容
	worldState, err := ParseWorldStateString(string(fileContent))
	if err != nil {
		return "", fmt.Errorf("解析世界状态文件失败: %v", err)
	}

	// 设置存档和世界名称
	worldState.ArchiveName = archiveName
	worldState.WorldName = worldName
	worldState.RawData = string(fileContent)

	// 将世界状态信息保存到数据库
	if err := models.SaveWorldStateInfo(worldState); err != nil {
		log.Printf("[WorldStateTask] 保存世界状态信息到数据库失败: %v", err)
		// 不返回错误，继续处理
	}

	// 更新文件缓存
	fileCacheMutex.Lock()
	fileCacheMap[worldStatePath] = fileCache{
		ModTime:     modTime,
		ContentMD5:  md5Str,
		PlayerCount: 0, // 不适用于世界状态
		FileSize:    fileSize,
	}
	fileCacheMutex.Unlock()

	// 构建返回消息
	message := fmt.Sprintf("成功读取世界状态文件，存档: %s, 世界: %s, 季节: %s, 天数: %d/%d",
		archiveName, worldName, worldState.Season, worldState.ElapsedDaysInSeason,
		worldState.ElapsedDaysInSeason+worldState.RemainingDaysInSeason)

	// 简化日志输出
	log.Printf("[WorldStateTask] 读取完成: %s/%s, 季节: %s, 天数: %d/%d",
		archiveName, worldName, worldState.Season, worldState.ElapsedDaysInSeason,
		worldState.ElapsedDaysInSeason+worldState.RemainingDaysInSeason)

	return message, nil
}

// ParseWorldStateString 解析世界状态字符串
func ParseWorldStateString(content string) (*models.WorldStateInfo, error) {
	// 创建世界状态信息对象
	worldState := &models.WorldStateInfo{}

	// 初始化字段计数器
	var fieldCount int

	// 正则表达式匹配世界状态行
	// 格式: KLEI     1 return {summerlength=15,cavemoonphase="threequarter",...}
	// 注意：有些文件可能没有KLEI前缀，直接是世界状态数据
	re := regexp.MustCompile(`(?:KLEI\s+\d+\s+return\s+)?\{(.*)\}`)

	// 匹配世界状态行
	matches := re.FindStringSubmatch(content)
	if len(matches) < 2 {
		log.Printf("[WorldStateTask] 内容不匹配世界状态格式: %s", content)
		return nil, fmt.Errorf("内容不匹配世界状态格式")
	}

	// 提取字段内容
	content = matches[1]

	// 定义正则表达式来匹配各种类型的字段
	// 匹配字符串类型的字段，如 season="autumn"
	stringFieldRe := regexp.MustCompile(`(\w+)="([^"]*)"`)
	// 匹配数值类型的字段，如 cycles=407
	numberFieldRe := regexp.MustCompile(`(\w+)=(-?\d+(?:\.\d+)?)`)
	// 匹配布尔类型的字段，如 isautumn=true
	boolFieldRe := regexp.MustCompile(`(\w+)=(true|false)`)

	// 查找所有字符串类型的字段
	stringMatches := stringFieldRe.FindAllStringSubmatch(content, -1)
	for _, match := range stringMatches {
		if len(match) >= 3 {
			fieldName := match[1]
			fieldValue := match[2]

			fieldCount++
			switch fieldName {
			case "season":
				worldState.Season = fieldValue
			case "phase":
				worldState.Phase = fieldValue
			case "moonphase":
				worldState.MoonPhase = fieldValue
			case "precipitation":
				worldState.Precipitation = fieldValue
			case "cavemoonphase":
				worldState.CaveMoonPhase = fieldValue
			case "cavephase":
				worldState.CavePhase = fieldValue
			case "nightmarephase":
				worldState.NightmarePhase = fieldValue
			}
		}
	}

	// 查找所有数值类型的字段
	numberMatches := numberFieldRe.FindAllStringSubmatch(content, -1)
	for _, match := range numberMatches {
		if len(match) >= 3 {
			fieldName := match[1]
			fieldValueStr := match[2]

			// 尝试将字符串转换为浮点数
			fieldValue, err := strconv.ParseFloat(fieldValueStr, 64)
			if err != nil {
				log.Printf("[WorldStateTask] 无法解析数值字段 %s: %v", fieldName, err)
				continue
			}

			fieldCount++
			switch fieldName {
			case "cycles":
				worldState.Cycles = int(fieldValue)
			case "elapseddaysinseason":
				worldState.ElapsedDaysInSeason = int(fieldValue)
			case "remainingdaysinseason":
				worldState.RemainingDaysInSeason = int(fieldValue)
			case "temperature":
				worldState.Temperature = fieldValue
			case "autumnlength":
				worldState.AutumnLength = int(fieldValue)
			case "winterlength":
				worldState.WinterLength = int(fieldValue)
			case "springlength":
				worldState.SpringLength = int(fieldValue)
			case "summerlength":
				worldState.SummerLength = int(fieldValue)
			case "seasonprogress":
				worldState.SeasonProgress = fieldValue
			case "time":
				worldState.Time = fieldValue
			case "timeinphase":
				worldState.TimeInPhase = fieldValue
			case "wetness":
				worldState.Wetness = fieldValue
			case "moisture":
				worldState.Moisture = fieldValue
			case "moistureceil":
				worldState.MoistureCeil = fieldValue
			case "pop":
				worldState.Pop = fieldValue
			case "snowlevel":
				worldState.SnowLevel = fieldValue
			case "lunarhaillevel":
				worldState.LunarHailLevel = fieldValue
			case "nightmaretime":
				worldState.NightmareTime = fieldValue
			case "nightmaretimeinphase":
				worldState.NightmareTimeInPhase = fieldValue
			case "precipitationrate":
				worldState.PrecipitationRate = fieldValue
			}
		}
	}

	// 查找所有布尔类型的字段
	boolMatches := boolFieldRe.FindAllStringSubmatch(content, -1)
	for _, match := range boolMatches {
		if len(match) >= 3 {
			fieldName := match[1]
			fieldValueStr := match[2]

			// 将字符串转换为布尔值
			fieldValue := fieldValueStr == "true"

			fieldCount++
			switch fieldName {
			case "isday":
				worldState.IsDay = fieldValue
			case "isdusk":
				worldState.IsDusk = fieldValue
			case "isnight":
				worldState.IsNight = fieldValue
			case "isautumn":
				worldState.IsAutumn = fieldValue
			case "iswinter":
				worldState.IsWinter = fieldValue
			case "isspring":
				worldState.IsSpring = fieldValue
			case "issummer":
				worldState.IsSummer = fieldValue
			case "issnowing":
				worldState.IsSnowing = fieldValue
			case "israining":
				worldState.IsRaining = fieldValue
			case "iswet":
				worldState.IsWet = fieldValue
			case "isacidraining":
				worldState.IsAcidRaining = fieldValue
			case "islunarhailing":
				worldState.IsLunarHailing = fieldValue
			case "isalterawake":
				worldState.IsAlterAwake = fieldValue
			case "isfullmoon":
				worldState.IsFullMoon = fieldValue
			case "isnewmoon":
				worldState.IsNewMoon = fieldValue
			case "iswaxingmoon":
				worldState.IsWaxingMoon = fieldValue
			case "issnowcovered":
				worldState.IsSnowCovered = fieldValue
			// 洞穴相关布尔字段
			case "iscaveday":
				worldState.IsCaveDay = fieldValue
			case "iscavedusk":
				worldState.IsCaveDusk = fieldValue
			case "iscavenight":
				worldState.IsCaveNight = fieldValue
			case "iscavefullmoon":
				worldState.IsCaveFullMoon = fieldValue
			case "iscavenewmoon":
				worldState.IsCaveNewMoon = fieldValue
			case "iscavewaxingmoon":
				worldState.IsCaveWaxingMoon = fieldValue
			// 噩梦相关布尔字段
			case "isnightmarecalm":
				worldState.IsNightmareCalm = fieldValue
			case "isnightmarewarn":
				worldState.IsNightmareWarn = fieldValue
			case "isnightmarewild":
				worldState.IsNightmareWild = fieldValue
			case "isnightmaredawn":
				worldState.IsNightmareDawn = fieldValue
			}
		}
	}

	// 记录解析到的字段数量
	log.Printf("[WorldStateTask] 成功解析世界状态文件，共解析了 %d 个字段", fieldCount)

	return worldState, nil
}

// TestParseWorldState 测试解析世界状态
func TestParseWorldState(content string) string {
	// 计算原始字段数量
	// 先提取字段内容
	re := regexp.MustCompile(`(?:KLEI\s+\d+\s+return\s+)?\{(.*)\}`)
	matches := re.FindStringSubmatch(content)
	var originalFieldCount int
	if len(matches) >= 2 {
		// 简单的按逗号分割计数
		originalFieldCount = len(strings.Split(matches[1], ","))
	}

	worldState, err := ParseWorldStateString(content)
	if err != nil {
		return fmt.Sprintf("解析失败: %v", err)
	}

	// 构建返回消息
	var result strings.Builder
	result.WriteString(fmt.Sprintf("解析成功! 原始字段数: %d\n", originalFieldCount))
	result.WriteString(fmt.Sprintf("季节: %s (进度: %.2f%%)\n", worldState.Season, worldState.SeasonProgress*100))
	result.WriteString(fmt.Sprintf("天数: %d/%d (总天数: %d)\n",
		worldState.ElapsedDaysInSeason,
		worldState.ElapsedDaysInSeason+worldState.RemainingDaysInSeason,
		worldState.Cycles))
	result.WriteString(fmt.Sprintf("时间: %s (进度: %.2f%%)\n", worldState.Phase, worldState.TimeInPhase*100))
	result.WriteString(fmt.Sprintf("温度: %.2f\n", worldState.Temperature))
	result.WriteString(fmt.Sprintf("月相: %s\n", worldState.MoonPhase))
	result.WriteString(fmt.Sprintf("降水: %s (概率: %.2f%%)\n", worldState.Precipitation, worldState.Pop*100))

	// 添加季节长度信息
	result.WriteString(fmt.Sprintf("季节长度: 秋=%d, 冬=%d, 春=%d, 夏=%d\n",
		worldState.AutumnLength, worldState.WinterLength,
		worldState.SpringLength, worldState.SummerLength))

	// 添加洞穴相关信息
	result.WriteString("\n洞穴信息:\n")
	result.WriteString(fmt.Sprintf("洞穴时间段: %s\n", worldState.CavePhase))
	result.WriteString(fmt.Sprintf("洞穴月相: %s\n", worldState.CaveMoonPhase))

	// 添加噩梦相关信息
	result.WriteString("\n噩梦信息:\n")
	result.WriteString(fmt.Sprintf("噩梦阶段: %s\n", worldState.NightmarePhase))
	result.WriteString(fmt.Sprintf("噩梦时间: %.2f (进度: %.2f%%)\n",
		worldState.NightmareTime, worldState.NightmareTimeInPhase*100))

	// 添加其他数值信息
	result.WriteString("\n其他数值信息:\n")
	result.WriteString(fmt.Sprintf("降水率: %.2f%%\n", worldState.PrecipitationRate*100))

	// 添加当前状态信息
	result.WriteString("\n当前状态:\n")

	// 主世界状态
	var states []string
	if worldState.IsDay {
		states = append(states, "白天")
	}
	if worldState.IsDusk {
		states = append(states, "黄昏")
	}
	if worldState.IsNight {
		states = append(states, "夜晚")
	}
	if worldState.IsAutumn {
		states = append(states, "秋季")
	}
	if worldState.IsWinter {
		states = append(states, "冬季")
	}
	if worldState.IsSpring {
		states = append(states, "春季")
	}
	if worldState.IsSummer {
		states = append(states, "夏季")
	}
	if worldState.IsSnowing {
		states = append(states, "下雪")
	}
	if worldState.IsRaining {
		states = append(states, "下雨")
	}
	if worldState.IsWet {
		states = append(states, "潮湿")
	}
	if worldState.IsAcidRaining {
		states = append(states, "酸雨")
	}
	if worldState.IsLunarHailing {
		states = append(states, "月岩冰雹")
	}
	if worldState.IsFullMoon {
		states = append(states, "满月")
	}
	if worldState.IsNewMoon {
		states = append(states, "新月")
	}
	if worldState.IsSnowCovered {
		states = append(states, "积雪")
	}
	result.WriteString(fmt.Sprintf("主世界: %s\n", strings.Join(states, ", ")))

	// 洞穴状态
	var caveStates []string
	if worldState.IsCaveDay {
		caveStates = append(caveStates, "白天")
	}
	if worldState.IsCaveDusk {
		caveStates = append(caveStates, "黄昏")
	}
	if worldState.IsCaveNight {
		caveStates = append(caveStates, "夜晚")
	}
	if worldState.IsCaveFullMoon {
		caveStates = append(caveStates, "满月")
	}
	if worldState.IsCaveNewMoon {
		caveStates = append(caveStates, "新月")
	}
	if worldState.IsCaveWaxingMoon {
		caveStates = append(caveStates, "渐盈月")
	}
	result.WriteString(fmt.Sprintf("洞穴世界: %s\n", strings.Join(caveStates, ", ")))

	// 噩梦状态
	var nightmareStates []string
	if worldState.IsNightmareCalm {
		nightmareStates = append(nightmareStates, "平静期")
	}
	if worldState.IsNightmareWarn {
		nightmareStates = append(nightmareStates, "警告期")
	}
	if worldState.IsNightmareWild {
		nightmareStates = append(nightmareStates, "狂暴期")
	}
	if worldState.IsNightmareDawn {
		nightmareStates = append(nightmareStates, "黎明期")
	}
	result.WriteString(fmt.Sprintf("噩梦状态: %s\n", strings.Join(nightmareStates, ", ")))

	return result.String()
}

// RegisterWorldStateTasks 注册世界状态相关任务
func RegisterWorldStateTasks(manager *TaskManager) {
	// 注册读取世界状态文件任务
	manager.RegisterFunctionWithInfo("read_world_state", ReadWorldStateFile, FunctionInfo{
		Name:        "read_world_state",
		Description: "读取世界状态文件",
		ParamTypes:  []string{"string", "string"}, // 存档名称, 世界名称
	})

	// 注册测试解析世界状态函数
	manager.RegisterFunctionWithInfo("test_parse_world_state", TestParseWorldState, FunctionInfo{
		Name:        "test_parse_world_state",
		Description: "测试解析世界状态字符串",
		ParamTypes:  []string{"string"}, // 配置字符串
	})

	// 创建默认的世界状态读取任务
	archives := GetArchiveList()
	log.Printf("[WorldStateTask] 找到 %d 个存档", len(archives))

	for _, archive := range archives {
		worlds := GetWorldList(archive)
		log.Printf("[WorldStateTask] 存档 %s 中找到 %d 个世界", archive, len(worlds))

		for _, world := range worlds {
			// 创建任务名称
			taskName := fmt.Sprintf("read_world_state_%s_%s", archive, world)

			// 检查世界状态文件是否存在
			dstSavePath := getDstSavePath()
			worldStatePath := filepath.Join(dstSavePath, archive, world, "save", "mod_config_data", "world_state")

			if _, err := os.Stat(worldStatePath); os.IsNotExist(err) {
				log.Printf("[WorldStateTask] 存档 %s 世界 %s 的世界状态文件不存在，跳过创建任务", archive, world)
				continue
			}

			// 检查任务是否已存在
			// 使用数据库查询检查任务是否存在
			tasks, err := models.GetAllTasks()
			taskExists := false
			if err == nil {
				for _, t := range tasks {
					if t.Name == taskName {
						taskExists = true
						break
					}
				}
			}

			if !taskExists {
				// 创建新任务
				task := models.CronTask{
					Name:        taskName,
					Description: fmt.Sprintf("读取存档 %s 世界 %s 的世界状态文件", archive, world),
					Spec:        "*/5 * * * * *", // 每5秒执行一次
					Command:     fmt.Sprintf("read_world_state(\"%s\", \"%s\")", archive, world),
					Status:      1, // 启用
					GroupID:     1, // 默认分组
				}

				// 保存任务到数据库
				if err := models.AddTask(&task); err != nil {
					log.Printf("[WorldStateTask] 创建任务失败: %v", err)
				} else {
					log.Printf("[WorldStateTask] 成功创建任务: %s", taskName)
				}
			} else {
				log.Printf("[WorldStateTask] 任务已存在: %s", taskName)
			}
		}
	}
}
