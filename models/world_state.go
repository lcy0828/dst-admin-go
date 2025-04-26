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
	ElapsedDaysInSeason   int       `json:"elapsed_days_in_season"`   // 当前季节已经过去的天数
	RemainingDaysInSeason int       `json:"remaining_days_in_season"` // 当前季节还剩下的天数
	IsDay                 bool      `json:"is_day"`                   // 当前是否是白天
	IsDusk                bool      `json:"is_dusk"`                  // 当前是否是黄昏
	IsNight               bool      `json:"is_night"`                 // 当前是否是夜晚
	IsAutumn              bool      `json:"is_autumn"`                // 当前季节是否是秋季
	IsWinter              bool      `json:"is_winter"`                // 当前季节是否是冬季
	IsSpring              bool      `json:"is_spring"`                // 当前季节是否是春季
	IsSummer              bool      `json:"is_summer"`                // 当前季节是否是夏季
	IsSnowing             bool      `json:"is_snowing"`               // 当前是否正在下雪
	IsRaining             bool      `json:"is_raining"`               // 当前是否正在下雨
	IsWet                 bool      `json:"is_wet"`                   // 世界环境当前是否普遍潮湿
	Temperature           float64   `json:"temperature"`              // 当前世界的环境温度
	Precipitation         string    `json:"precipitation"`            // 当前的降水类型 (none, rain, snow)
	MoonPhase             string    `json:"moon_phase"`               // 当前月相
	AutumnLength          int       `json:"autumn_length"`            // 秋季设定持续的总天数
	WinterLength          int       `json:"winter_length"`            // 冬季设定持续的总天数
	SpringLength          int       `json:"spring_length"`            // 春季设定持续的总天数
	SummerLength          int       `json:"summer_length"`            // 夏季设定持续的总天数
	SeasonProgress        float64   `json:"season_progress"`          // 当前季节的进度 (0-1)
	Time                  float64   `json:"time"`                     // 当前在整个昼夜循环中的进度 (0-1)
	TimeInPhase           float64   `json:"time_in_phase"`            // 当前在当前时间段内的进度 (0-1)
	Wetness               float64   `json:"wetness"`                  // 潮湿度
	Moisture              float64   `json:"moisture"`                 // 湿度值
	MoistureCeil          float64   `json:"moisture_ceil"`            // 湿度上限值
	Pop                   float64   `json:"pop"`                      // 降水概率
	SnowLevel             float64   `json:"snow_level"`               // 积雪程度
	IsAcidRaining         bool      `json:"is_acid_raining"`          // 当前是否正在下酸雨
	IsLunarHailing        bool      `json:"is_lunar_hailing"`         // 当前是否正在下月岩冰雹
	LunarHailLevel        float64   `json:"lunar_hail_level"`         // 月岩冰雹的强度等级
	IsAlterAwake          bool      `json:"is_alter_awake"`           // 月亮祭坛是否处于激活状态
	IsFullMoon            bool      `json:"is_full_moon"`             // 当前是否是满月
	IsNewMoon             bool      `json:"is_new_moon"`              // 当前是否是新月
	IsWaxingMoon          bool      `json:"is_waxing_moon"`           // 月亮当前是否处于渐盈状态
	IsSnowCovered         bool      `json:"is_snow_covered"`          // 地表是否被雪覆盖
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
func SaveWorldStateInfo(worldState *WorldStateInfo) error {
	// 检查是否已存在相同存档和世界的记录
	var existingState WorldStateInfo
	result := db.Where("archive_name = ? AND world_name = ?",
		worldState.ArchiveName, worldState.WorldName).
		Order("created_at DESC").
		First(&existingState)

	// 如果存在记录，检查是否有变化
	if result.Error == nil {
		// 如果原始数据相同，不需要更新
		if existingState.RawData == worldState.RawData {
			log.Printf("[WorldStateInfo] 世界状态未变化，跳过更新: %s/%s",
				worldState.ArchiveName, worldState.WorldName)
			return nil
		}
	} else if !gorm.IsRecordNotFoundError(result.Error) {
		// 如果是其他错误，返回错误
		return result.Error
	}

	// 设置创建和更新时间
	now := time.Now()
	worldState.CreatedAt = now
	worldState.UpdatedAt = now

	// 创建新记录
	if err := db.Create(worldState).Error; err != nil {
		log.Printf("[WorldStateInfo] 保存世界状态信息失败: %v", err)
		return err
	}

	log.Printf("[WorldStateInfo] 成功保存世界状态信息: %s/%s, 季节=%s, 天数=%d/%d",
		worldState.ArchiveName, worldState.WorldName,
		worldState.Season, worldState.ElapsedDaysInSeason,
		worldState.ElapsedDaysInSeason+worldState.RemainingDaysInSeason)
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
func GetWorldStateHistory(archiveName, worldName string, limit, offset int) ([]WorldStateInfo, error) {
	var worldStates []WorldStateInfo
	query := db.Where("archive_name = ? AND world_name = ?", archiveName, worldName).
		Order("created_at DESC")

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
