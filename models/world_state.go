package models

import (
	"log"
	"time"

	"github.com/jinzhu/gorm"
)

// WorldStateInfo 世界状态信息表
type WorldStateInfo struct {
	ID                    int       `gorm:"primary_key" json:"id"`
	ArchiveName           string    `json:"archive_name"`             // 存档名称
	WorldName             string    `json:"world_name"`               // 世界名称
	Season                string    `json:"season"`                   // 当前季节 (autumn, winter, spring, summer)
	Phase                 string    `json:"phase"`                    // 当前时间段 (day, dusk, night)
	Cycles                int       `json:"cycles"`                   // 已经过的完整昼夜循环总数
	ElapsedDaysInSeason   int       `json:"elapseddaysinseason"`      // 当前季节已经过去的天数
	RemainingDaysInSeason int       `json:"remainingdaysinseason"`    // 当前季节还剩下的天数
	IsDay                 bool      `json:"isday"`                    // 当前是否是白天
	IsDusk                bool      `json:"isdusk"`                   // 当前是否是黄昏
	IsNight               bool      `json:"isnight"`                  // 当前是否是夜晚
	IsAutumn              bool      `json:"isautumn"`                 // 当前季节是否是秋季
	IsWinter              bool      `json:"iswinter"`                 // 当前季节是否是冬季
	IsSpring              bool      `json:"isspring"`                 // 当前季节是否是春季
	IsSummer              bool      `json:"issummer"`                 // 当前季节是否是夏季
	IsSnowing             bool      `json:"issnowing"`                // 当前是否正在下雪
	IsRaining             bool      `json:"israining"`                // 当前是否正在下雨
	IsWet                 bool      `json:"iswet"`                    // 世界环境当前是否普遍潮湿
	Temperature           float64   `json:"temperature"`              // 当前世界的环境温度
	Precipitation         string    `json:"precipitation"`            // 当前的降水类型 (none, rain, snow)
	MoonPhase             string    `json:"moonphase"`                // 当前月相
	AutumnLength          int       `json:"autumnlength"`             // 秋季设定持续的总天数
	WinterLength          int       `json:"winterlength"`             // 冬季设定持续的总天数
	SpringLength          int       `json:"springlength"`             // 春季设定持续的总天数
	SummerLength          int       `json:"summerlength"`             // 夏季设定持续的总天数
	SeasonProgress        float64   `json:"seasonprogress"`           // 当前季节的进度 (0-1)
	Time                  float64   `json:"time"`                     // 当前在整个昼夜循环中的进度 (0-1)
	TimeInPhase           float64   `json:"timeinphase"`              // 当前在当前时间段内的进度 (0-1)
	Wetness               float64   `json:"wetness"`                  // 潮湿度
	Moisture              float64   `json:"moisture"`                 // 湿度值
	MoistureCeil          float64   `json:"moistureceil"`            // 湿度上限值
	Pop                   float64   `json:"pop"`                      // 降水概率
	SnowLevel             float64   `json:"snowlevel"`               // 积雪程度
	IsAcidRaining         bool      `json:"isacidraining"`           // 当前是否正在下酸雨
	IsLunarHailing        bool      `json:"islunarhailing"`          // 当前是否正在下月岩冰雹
	LunarHailLevel        float64   `json:"lunarhaillevel"`          // 月岩冰雹的强度等级
	IsAlterAwake          bool      `json:"isalterawake"`            // 月亮祭坛是否处于激活状态
	IsFullMoon            bool      `json:"isfullmoon"`              // 当前是否是满月
	IsNewMoon             bool      `json:"isnewmoon"`               // 当前是否是新月
	IsWaxingMoon          bool      `json:"iswaxingmoon"`            // 月亮当前是否处于渐盈状态
	IsSnowCovered         bool      `json:"issnowcovered"`           // 地表是否被雪覆盖
	// 洞穴相关字段
	CaveMoonPhase         string    `json:"cavemoonphase"`           // 洞穴月相
	CavePhase             string    `json:"cavephase"`                // 洞穴时间段 (day, dusk, night)
	IsCaveDay             bool      `json:"iscaveday"`               // 洞穴是否是白天
	IsCaveDusk            bool      `json:"iscavedusk"`              // 洞穴是否是黄昏
	IsCaveNight           bool      `json:"iscavenight"`             // 洞穴是否是夜晚
	IsCaveFullMoon        bool      `json:"iscavefullmoon"`          // 洞穴是否是满月
	IsCaveNewMoon         bool      `json:"iscavenewmoon"`           // 洞穴是否是新月
	IsCaveWaxingMoon      bool      `json:"iscavewaxingmoon"`        // 洞穴月亮是否处于渐盈状态
	// 噩梦相关字段
	NightmarePhase        string    `json:"nightmarephase"`           // 噩梦阶段 (none, calm, warn, wild, dawn)
	NightmareTime         float64   `json:"nightmaretime"`           // 噩梦时间
	NightmareTimeInPhase  float64   `json:"nightmaretimeinphase"`      // 当前噩梦阶段内的进度
	IsNightmareCalm       bool      `json:"isnightmarecalm"`         // 是否处于噩梦平静期
	IsNightmareWarn       bool      `json:"isnightmarewarn"`         // 是否处于噩梦警告期
	IsNightmareWild       bool      `json:"isnightmarewild"`         // 是否处于噩梦狂暴期
	IsNightmareDawn       bool      `json:"isnightmaredawn"`         // 是否处于噩梦黎明期
	// 其他字段
	PrecipitationRate     float64   `json:"precipitationrate"`        // 降水率
	RawData               string    `json:"raw_data"`                 // 原始数据
	CreatedAt             time.Time `json:"created_at"`               // 记录创建时间
	UpdatedAt             time.Time `json:"updated_at"`               // 记录更新时间
}

// TableName 设置表名
func (WorldStateInfo) TableName() string {
	return "dont_world_state_info"
}

// InitWorldStateInfoTable 初始化世界状态信息表
func InitWorldStateInfoTable() {
	log.Println("[WorldStateInfo] 开始初始化世界状态信息表")

	// 检查表是否存在
	hasTable := db.HasTable(&WorldStateInfo{})
	log.Printf("[WorldStateInfo] 检查表是否存在: %v", hasTable)

	// 自动迁移表结构
	if err := db.AutoMigrate(&WorldStateInfo{}).Error; err != nil {
		log.Printf("[WorldStateInfo] 表结构迁移失败: %v", err)
	} else {
		log.Println("[WorldStateInfo] 表结构迁移成功")
	}

	// 添加索引以提高查询性能
	// 为 archive_name 和 world_name 添加组合索引
	if err := db.Model(&WorldStateInfo{}).AddIndex("idx_world_state_archive_world", "archive_name", "world_name").Error; err != nil {
		log.Printf("[WorldStateInfo] 添加索引 idx_world_state_archive_world 失败: %v", err)
	} else {
		log.Println("[WorldStateInfo] 添加索引 idx_world_state_archive_world 成功")
	}

	// 为 created_at 添加索引，用于查询最新记录
	if err := db.Model(&WorldStateInfo{}).AddIndex("idx_world_state_created_at", "created_at").Error; err != nil {
		log.Printf("[WorldStateInfo] 添加索引 idx_world_state_created_at 失败: %v", err)
	} else {
		log.Println("[WorldStateInfo] 添加索引 idx_world_state_created_at 成功")
	}
}

// SaveWorldStateInfo 保存世界状态信息到数据库
// 每个存档和世界只保留一条最新的记录
func SaveWorldStateInfo(worldState *WorldStateInfo) error {
	// 检查是否已存在相同存档和世界的记录
	var existingState WorldStateInfo
	result := db.Where("archive_name = ? AND world_name = ?",
		worldState.ArchiveName, worldState.WorldName).
		Order("created_at DESC").
		First(&existingState)

	// 设置更新时间
	now := time.Now()
	worldState.UpdatedAt = now

	// 如果存在记录
	if result.Error == nil {
		// 如果原始数据相同，不需要更新
		if existingState.RawData == worldState.RawData {
			log.Printf("[WorldStateInfo] 世界状态未变化，跳过更新: %s/%s",
				worldState.ArchiveName, worldState.WorldName)
			return nil
		}

		// 保留原有记录的ID和创建时间
		worldState.ID = existingState.ID
		worldState.CreatedAt = existingState.CreatedAt

		// 更新现有记录
		if err := db.Save(worldState).Error; err != nil {
			log.Printf("[WorldStateInfo] 更新世界状态信息失败: %v", err)
			return err
		}

		log.Printf("[WorldStateInfo] 成功更新世界状态信息: %s/%s, 季节=%s, 天数=%d/%d",
			worldState.ArchiveName, worldState.WorldName,
			worldState.Season, worldState.ElapsedDaysInSeason,
			worldState.ElapsedDaysInSeason+worldState.RemainingDaysInSeason)
	} else if gorm.IsRecordNotFoundError(result.Error) {
		// 如果记录不存在，创建新记录
		worldState.CreatedAt = now

		// 创建新记录
		if err := db.Create(worldState).Error; err != nil {
			log.Printf("[WorldStateInfo] 保存世界状态信息失败: %v", err)
			return err
		}

		log.Printf("[WorldStateInfo] 成功创建世界状态信息: %s/%s, 季节=%s, 天数=%d/%d",
			worldState.ArchiveName, worldState.WorldName,
			worldState.Season, worldState.ElapsedDaysInSeason,
			worldState.ElapsedDaysInSeason+worldState.RemainingDaysInSeason)
	} else {
		// 如果是其他错误，返回错误
		return result.Error
	}

	return nil
}

// GetLatestWorldStateInfo 获取最新的世界状态信息
func GetLatestWorldStateInfo(archiveName, worldName string) (*WorldStateInfo, error) {
	var worldState WorldStateInfo
	result := db.Where("archive_name = ? AND world_name = ?",
		archiveName, worldName).
		Order("created_at DESC").
		First(&worldState)

	if result.Error != nil {
		if gorm.IsRecordNotFoundError(result.Error) {
			log.Printf("[WorldStateInfo] 未找到世界状态信息: %s/%s", archiveName, worldName)
			return nil, nil
		}
		log.Printf("[WorldStateInfo] 获取世界状态信息失败: %v", result.Error)
		return nil, result.Error
	}

	return &worldState, nil
}

// GetWorldStateHistory 获取世界状态历史记录
// 注意：由于现在每个存档和世界只保留一条记录，所以这个函数现在返回所有存档和世界的最新状态
func GetWorldStateHistory(archiveName, worldName string, limit, offset int) ([]WorldStateInfo, error) {
	var worldStates []WorldStateInfo
	var query *gorm.DB

	// 如果指定了存档和世界，只返回该存档和世界的记录
	if archiveName != "" && worldName != "" {
		// 直接获取最新的一条记录
		worldState, err := GetLatestWorldStateInfo(archiveName, worldName)
		if err != nil {
			return nil, err
		}
		if worldState != nil {
			worldStates = append(worldStates, *worldState)
		}
		return worldStates, nil
	} else if archiveName != "" {
		// 如果只指定了存档，返回该存档下所有世界的记录
		query = db.Where("archive_name = ?", archiveName)
	} else {
		// 如果没有指定存档和世界，返回所有记录
		query = db
	}

	// 添加排序和分页
	query = query.Order("archive_name ASC, world_name ASC")

	if limit > 0 {
		query = query.Limit(limit)
	}
	if offset > 0 {
		query = query.Offset(offset)
	}

	if err := query.Find(&worldStates).Error; err != nil {
		log.Printf("[WorldStateInfo] 获取世界状态历史记录失败: %v", err)
		return nil, err
	}

	return worldStates, nil
}
