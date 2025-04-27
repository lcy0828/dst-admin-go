package models

import (
	"log"
	"time"

	"github.com/jinzhu/gorm"
)

// HostInfo 主机信息表
type HostInfo struct {
	ID             int       `gorm:"primary_key" json:"id"`
	ArchiveName    string    `json:"archive_name"`    // 存档名称
	WorldName      string    `json:"world_name"`      // 世界名称
	UserID         string    `json:"user_id"`         // 用户ID (KU_xxx格式)
	Name           string    `json:"name"`            // 主机名称，通常是 [Host]
	Performance    int       `json:"performance"`     // 性能指标
	EventLevel     int       `json:"event_level"`     // 事件等级
	UserFlags      int       `json:"user_flags"`      // 用户标志
	ColourR        float32   `json:"colour_r"`        // 颜色R分量
	ColourG        float32   `json:"colour_g"`        // 颜色G分量
	ColourB        float32   `json:"colour_b"`        // 颜色B分量
	ColourA        float32   `json:"colour_a"`        // 颜色A分量
	SkillSelection string    `json:"skill_selection"` // 技能选择，以JSON字符串存储
	Vanity         string    `json:"vanity"`          // 装饰性物品，以JSON字符串存储
	Equip          string    `json:"equip"`           // 装备物品，以JSON字符串存储
	FirstSeen      time.Time `json:"first_seen"`      // 首次出现时间
	LastSeen       time.Time `json:"last_seen"`       // 最后出现时间
	CreatedAt      time.Time `json:"created_at"`      // 记录创建时间
	UpdatedAt      time.Time `json:"updated_at"`      // 记录更新时间
}

// TableName 设置表名
func (HostInfo) TableName() string {
	return "host_info"
}

// InitHostInfoTable 初始化主机信息表
func InitHostInfoTable() {
	log.Println("[HostInfo] 开始初始化主机信息表")

	// 检查表是否存在
	hasTable := db.HasTable(&HostInfo{})
	log.Printf("[HostInfo] 检查表是否存在: %v", hasTable)

	// 自动迁移表结构
	if err := db.AutoMigrate(&HostInfo{}).Error; err != nil {
		log.Printf("[HostInfo] 表结构迁移失败: %v", err)
	} else {
		log.Println("[HostInfo] 表结构迁移成功")
	}

	// 添加索引以提高查询性能
	// 为 archive_name 和 world_name 添加组合索引
	if err := db.Model(&HostInfo{}).AddIndex("idx_host_info_archive_world", "archive_name", "world_name").Error; err != nil {
		log.Printf("[HostInfo] 添加索引 idx_host_info_archive_world 失败: %v", err)
	} else {
		log.Println("[HostInfo] 添加索引 idx_host_info_archive_world 成功")
	}

	// 为 archive_name 和 user_id 添加唯一索引
	if err := db.Model(&HostInfo{}).AddUniqueIndex("idx_host_info_archive_userid", "archive_name", "user_id").Error; err != nil {
		log.Printf("[HostInfo] 添加唯一索引 idx_host_info_archive_userid 失败: %v", err)
	} else {
		log.Println("[HostInfo] 添加唯一索引 idx_host_info_archive_userid 成功")
	}

	// 为 last_seen 添加索引
	if err := db.Model(&HostInfo{}).AddIndex("idx_host_info_last_seen", "last_seen").Error; err != nil {
		log.Printf("[HostInfo] 添加索引 idx_host_info_last_seen 失败: %v", err)
	} else {
		log.Println("[HostInfo] 添加索引 idx_host_info_last_seen 成功")
	}

	log.Println("[HostInfo] 主机信息表初始化完成")
}

// SaveHostInfo 保存主机信息到数据库
func SaveHostInfo(hostInfo *HostInfo) error {
	// 查询主机是否已存在 - 根据存档名称和用户ID判断唯一性
	var existingHost HostInfo
	result := db.Where("archive_name = ? AND user_id = ?",
		hostInfo.ArchiveName, hostInfo.UserID).First(&existingHost)

	if result.Error != nil && !gorm.IsRecordNotFoundError(result.Error) {
		// 查询出错，但不是因为记录不存在
		return result.Error
	} else if gorm.IsRecordNotFoundError(result.Error) {
		// 主机不存在，创建新记录
		hostInfo.FirstSeen = time.Now()
		hostInfo.LastSeen = time.Now()
		hostInfo.CreatedAt = time.Now()
		hostInfo.UpdatedAt = time.Now()

		if err := db.Create(hostInfo).Error; err != nil {
			return err
		}

		log.Printf("[HostInfo] 创建新主机记录: UserID=%s, 存档=%s",
			hostInfo.UserID, hostInfo.ArchiveName)
	} else {
		// 主机已存在，更新记录
		existingHost.Name = hostInfo.Name
		existingHost.Performance = hostInfo.Performance
		existingHost.EventLevel = hostInfo.EventLevel
		existingHost.UserFlags = hostInfo.UserFlags
		existingHost.ColourR = hostInfo.ColourR
		existingHost.ColourG = hostInfo.ColourG
		existingHost.ColourB = hostInfo.ColourB
		existingHost.ColourA = hostInfo.ColourA
		existingHost.SkillSelection = hostInfo.SkillSelection
		existingHost.Vanity = hostInfo.Vanity
		existingHost.Equip = hostInfo.Equip
		existingHost.LastSeen = time.Now()
		existingHost.UpdatedAt = time.Now()

		if err := db.Save(&existingHost).Error; err != nil {
			return err
		}

		log.Printf("[HostInfo] 更新主机记录: UserID=%s, 存档=%s",
			hostInfo.UserID, hostInfo.ArchiveName)
	}

	return nil
}

// GetHostInfo 获取主机信息
func GetHostInfo(archiveName, worldName string) ([]HostInfo, error) {
	var hosts []HostInfo
	query := db.Model(&HostInfo{})

	// 添加存档名称条件
	if archiveName != "" {
		query = query.Where("archive_name = ?", archiveName)
	}

	// 添加世界名称条件
	if worldName != "" {
		query = query.Where("world_name = ?", worldName)
	}

	// 执行查询
	err := query.Order("last_seen DESC").Find(&hosts).Error
	if err != nil {
		log.Printf("[HostInfo] 获取主机信息失败: %v", err)
		return nil, err
	}

	log.Printf("[HostInfo] 成功获取 %d 个主机信息", len(hosts))
	return hosts, nil
}

// GetHostByID 根据ID获取主机信息
func GetHostByID(id int) (*HostInfo, error) {
	var host HostInfo
	err := db.Where("id = ?", id).First(&host).Error
	if err != nil {
		return nil, err
	}
	return &host, nil
}

// DeleteHostInfo 删除主机信息
func DeleteHostInfo(id int) error {
	return db.Where("id = ?", id).Delete(&HostInfo{}).Error
}

// DeleteDuplicateHostInfo 删除重复的主机信息记录
// 根据archive_name和user_id判断唯一性，保留最新的记录
func DeleteDuplicateHostInfo() error {
	log.Println("[HostInfo] 开始删除重复的主机信息记录")

	// 查询所有主机信息记录，按archive_name和user_id分组
	rows, err := db.Raw(`
		SELECT h1.id, h1.archive_name, h1.user_id
		FROM dont_host_info h1
		INNER JOIN (
			SELECT archive_name, user_id, MAX(id) as max_id
			FROM dont_host_info
			GROUP BY archive_name, user_id
			HAVING COUNT(*) > 1
		) h2 ON h1.archive_name = h2.archive_name AND h1.user_id = h2.user_id AND h1.id != h2.max_id
	`).Rows()

	if err != nil {
		log.Printf("[HostInfo] 查询重复记录失败: %v", err)
		return err
	}
	defer rows.Close()

	// 删除重复记录
	var duplicateIDs []int
	for rows.Next() {
		var id int
		var archiveName, userID string
		if err := rows.Scan(&id, &archiveName, &userID); err != nil {
			log.Printf("[HostInfo] 扫描行数据失败: %v", err)
			continue
		}
		duplicateIDs = append(duplicateIDs, id)
		log.Printf("[HostInfo] 发现重复记录: ID=%d, 存档=%s, 用户ID=%s", id, archiveName, userID)
	}

	// 删除重复记录
	if len(duplicateIDs) > 0 {
		for _, id := range duplicateIDs {
			if err := DeleteHostInfo(id); err != nil {
				log.Printf("[HostInfo] 删除重复记录失败: ID=%d, 错误=%v", id, err)
			} else {
				log.Printf("[HostInfo] 成功删除重复记录: ID=%d", id)
			}
		}
	} else {
		log.Println("[HostInfo] 未发现重复记录")
	}

	return nil
}

// RebuildHostInfoTable 重建主机信息表结构
// 这个函数会删除旧的唯一索引，并添加新的唯一索引
func RebuildHostInfoTable() error {
	log.Println("[HostInfo] 开始重建主机信息表结构")

	// 删除旧的唯一索引
	if err := db.Exec("DROP INDEX IF EXISTS idx_host_info_archive_world_userid").Error; err != nil {
		log.Printf("[HostInfo] 删除旧的唯一索引失败: %v", err)
		return err
	}
	log.Println("[HostInfo] 成功删除旧的唯一索引")

	// 添加新的唯一索引
	if err := db.Model(&HostInfo{}).AddUniqueIndex("idx_host_info_archive_userid", "archive_name", "user_id").Error; err != nil {
		log.Printf("[HostInfo] 添加新的唯一索引失败: %v", err)
		return err
	}
	log.Println("[HostInfo] 成功添加新的唯一索引")

	// 删除重复记录
	if err := DeleteDuplicateHostInfo(); err != nil {
		log.Printf("[HostInfo] 删除重复记录失败: %v", err)
		return err
	}

	log.Println("[HostInfo] 主机信息表结构重建完成")
	return nil
}
