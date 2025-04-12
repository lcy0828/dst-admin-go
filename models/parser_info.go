package models

import (
	"log"
	"time"
)

// ParserInfo 日志解析器信息
type ParserInfo struct {
	ID           int       `gorm:"primary_key" json:"id"`
	ArchiveName  string    `json:"archive_name"`  // 存档名称
	WorldName    string    `json:"world_name"`    // 世界名称
	StartTime    time.Time `json:"start_time"`    // 解析器启动时间
	LastActivity time.Time `json:"last_activity"` // 最后活动时间
	CreatedAt    time.Time `json:"created_at"`    // 记录创建时间
}

// InitParserInfoTable 初始化解析器信息表
func InitParserInfoTable() {
	// 自动迁移表结构
	db.AutoMigrate(&ParserInfo{})
	log.Println("解析器信息表初始化完成")
}

// GetParserStartTime 获取解析器启动时间
func GetParserStartTime(archiveName, worldName string) (time.Time, error) {
	var parserInfo ParserInfo

	// 创建东八区时区
	cst := time.FixedZone("CST", 8*3600)

	// 查询数据库
	result := db.Where("archive_name = ? AND world_name = ?", archiveName, worldName).First(&parserInfo)
	if result.Error != nil {
		// 如果记录不存在，创建一个新记录
		now := time.Now().In(cst)
		parserInfo = ParserInfo{
			ArchiveName:  archiveName,
			WorldName:    worldName,
			StartTime:    now,
			LastActivity: now,
			CreatedAt:    now,
		}

		if err := db.Create(&parserInfo).Error; err != nil {
			log.Printf("[Models] 创建解析器信息记录失败: %v", err)
			return now, err
		}

		return now, nil
	}

	return parserInfo.StartTime, nil
}

// UpdateParserLastActivity 更新解析器最后活动时间
func UpdateParserLastActivity(archiveName, worldName string) error {
	// 创建东八区时区
	cst := time.FixedZone("CST", 8*3600)
	now := time.Now().In(cst)

	// 查询数据库
	var parserInfo ParserInfo
	result := db.Where("archive_name = ? AND world_name = ?", archiveName, worldName).First(&parserInfo)

	if result.Error != nil {
		// 如果记录不存在，创建一个新记录
		parserInfo = ParserInfo{
			ArchiveName:  archiveName,
			WorldName:    worldName,
			StartTime:    now,
			LastActivity: now,
			CreatedAt:    now,
		}

		if err := db.Create(&parserInfo).Error; err != nil {
			log.Printf("[Models] 创建解析器信息记录失败: %v", err)
			return err
		}

		return nil
	}

	// 更新最后活动时间
	if err := db.Model(&parserInfo).Update("last_activity", now).Error; err != nil {
		log.Printf("[Models] 更新解析器最后活动时间失败: %v", err)
		return err
	}

	return nil
}
