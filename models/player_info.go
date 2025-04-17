package models

import (
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// PlayerStatus 玩家状态
const (
	PlayerStatusOnline  = "online"  // 在线
	PlayerStatusOffline = "offline" // 离线
)

// PlayerInfo 玩家信息表
type PlayerInfo struct {
	ID           int       `gorm:"primary_key" json:"id"`
	ArchiveName  string    `json:"archive_name"`  // 存档名称
	UserID       string    `json:"user_id"`       // 玩家ID (KU_xxx格式)
	PlayerName   string    `json:"player_name"`   // 玩家名称
	PlayerAge    int       `json:"player_age"`    // 玩家年龄
	Prefab       string    `json:"prefab"`        // 玩家角色
	Status       string    `json:"status"`        // 玩家状态：online/offline
	FirstSeen    time.Time `json:"first_seen"`    // 首次出现时间
	LastSeen     time.Time `json:"last_seen"`     // 最后出现时间
	StatusChange time.Time `json:"status_change"` // 状态变更时间
	CreatedAt    time.Time `json:"created_at"`    // 记录创建时间
	UpdatedAt    time.Time `json:"updated_at"`    // 记录更新时间
}

// InitPlayerInfoTable 初始化玩家信息表
func InitPlayerInfoTable() {
	log.Println("[PlayerInfo] 开始初始化玩家信息表")

	// 检查表是否存在
	hasTable := db.HasTable(&PlayerInfo{})
	log.Printf("[PlayerInfo] 检查表是否存在: %v", hasTable)

	// 自动迁移表结构
	if err := db.AutoMigrate(&PlayerInfo{}).Error; err != nil {
		log.Printf("[PlayerInfo] 表结构迁移失败: %v", err)
	} else {
		log.Println("[PlayerInfo] 表结构迁移成功")
	}

	// 添加索引以提高查询性能
	if err := db.Model(&PlayerInfo{}).AddIndex("idx_player_info_archive_userid", "archive_name", "user_id").Error; err != nil {
		log.Printf("[PlayerInfo] 添加索引 idx_player_info_archive_userid 失败: %v", err)
	} else {
		log.Println("[PlayerInfo] 添加索引 idx_player_info_archive_userid 成功")
	}

	if err := db.Model(&PlayerInfo{}).AddIndex("idx_player_info_status", "status").Error; err != nil {
		log.Printf("[PlayerInfo] 添加索引 idx_player_info_status 失败: %v", err)
	} else {
		log.Println("[PlayerInfo] 添加索引 idx_player_info_status 成功")
	}

	if err := db.Model(&PlayerInfo{}).AddIndex("idx_player_info_last_seen", "last_seen").Error; err != nil {
		log.Printf("[PlayerInfo] 添加索引 idx_player_info_last_seen 失败: %v", err)
	} else {
		log.Println("[PlayerInfo] 添加索引 idx_player_info_last_seen 成功")
	}

	log.Println("[PlayerInfo] 玩家信息表初始化完成")
}

// ParsePlayerListLog 解析玩家列表日志
// 解析格式: [DST-ADMIN-GO] [Listplayers] [0] [000] [KU_HQp7BOVs] [[Host]] []
func ParsePlayerListLog(rawContent string) []PlayerInfo {
	log.Printf("[PlayerInfo] 开始解析玩家列表日志")
	var players []PlayerInfo

	// 分行处理
	lines := strings.Split(rawContent, "\n")
	log.Printf("[PlayerInfo] 分行后共有 %d 行", len(lines))

	// 正则表达式匹配玩家信息行
	// 格式: [DST-ADMIN-GO] [Listplayers] [index] [age] [userid] [name] [prefab]
	re := regexp.MustCompile(`\[DST-ADMIN-GO\] \[Listplayers\] \[(\d+)\] \[(\d+)\] \[([^\]]+)\] \[([^\]]+)\] \[([^\]]*)\]`)

	for i, line := range lines {
		log.Printf("[PlayerInfo] 处理第 %d 行: %s", i+1, line)
		matches := re.FindStringSubmatch(line)
		if len(matches) >= 6 {
			log.Printf("[PlayerInfo] 匹配到玩家信息行: %s", line)

			// 提取玩家信息
			indexStr := matches[1]
			ageStr := matches[2]
			userID := matches[3]
			playerName := matches[4]
			prefab := matches[5]

			log.Printf("[PlayerInfo] 解析到玩家信息: 索引=%s, 年龄=%s, 用户ID=%s, 名称=%s, 角色=%s",
				indexStr, ageStr, userID, playerName, prefab)

			// 转换年龄为整数
			age, _ := strconv.Atoi(ageStr)

			// 跳过主机行
			if playerName == "[Host]" {
				log.Printf("[PlayerInfo] 跳过主机行")
				continue
			}

			// 创建玩家信息对象
			player := PlayerInfo{
				UserID:     userID,
				PlayerName: playerName,
				PlayerAge:  age,
				Prefab:     prefab,
				Status:     PlayerStatusOnline,
			}

			log.Printf("[PlayerInfo] 添加玩家: %s (%s)", player.PlayerName, player.UserID)
			players = append(players, player)
		} else {
			log.Printf("[PlayerInfo] 未匹配到玩家信息行: %s", line)
		}
	}

	log.Printf("[PlayerInfo] 解析完成，共有 %d 个玩家", len(players))

	return players
}

// UpdatePlayersFromLog 从日志更新玩家信息
func UpdatePlayersFromLog(archiveName string, rawContent string) error {
	log.Printf("[PlayerInfo] 开始从日志更新玩家信息, 存档: %s", archiveName)
	log.Printf("[PlayerInfo] 日志内容: %s", rawContent)

	// 解析日志获取当前在线玩家
	currentPlayers := ParsePlayerListLog(rawContent)
	log.Printf("[PlayerInfo] 解析到 %d 个玩家", len(currentPlayers))

	if len(currentPlayers) == 0 {
		log.Printf("[PlayerInfo] 没有解析到玩家信息，不需要更新")
		return nil // 没有玩家，不需要更新
	}

	// 获取当前时间
	now := time.Now()

	// 开启事务
	tx := db.Begin()
	if tx.Error != nil {
		return tx.Error
	}

	// 获取该存档中所有玩家的当前状态
	var existingPlayers []PlayerInfo
	if err := tx.Where("archive_name = ?", archiveName).Find(&existingPlayers).Error; err != nil {
		tx.Rollback()
		return err
	}

	// 创建现有玩家的映射，方便查找
	existingPlayerMap := make(map[string]PlayerInfo)
	// 检查是否有重复的UserID
	userIDCount := make(map[string]int)
	for _, player := range existingPlayers {
		userIDCount[player.UserID]++
		existingPlayerMap[player.UserID] = player
	}

	// 处理重复的UserID
	for userID, count := range userIDCount {
		if count > 1 {
			log.Printf("[PlayerInfo] 发现重复的UserID: %s, 存档: %s, 数量: %d", userID, archiveName, count)
			// 保留最新的一条记录，删除其他重复记录
			var duplicates []PlayerInfo
			if err := tx.Where("archive_name = ? AND user_id = ?", archiveName, userID).Order("last_seen DESC").Find(&duplicates).Error; err != nil {
				tx.Rollback()
				return err
			}

			// 保留第一条（最新的），删除其他的
			for i := 1; i < len(duplicates); i++ {
				if err := tx.Delete(&duplicates[i]).Error; err != nil {
					tx.Rollback()
					return err
				}
				log.Printf("[PlayerInfo] 删除重复记录: ID=%d, UserID=%s, 存档=%s",
					duplicates[i].ID, duplicates[i].UserID, duplicates[i].ArchiveName)
			}

			// 更新映射中的玩家信息为保留的记录
			existingPlayerMap[userID] = duplicates[0]
		}
	}

	// 处理当前在线的玩家
	for _, player := range currentPlayers {
		player.ArchiveName = archiveName

		// 检查玩家是否已存在
		existingPlayer, exists := existingPlayerMap[player.UserID]

		if exists {
			// 玩家已存在，更新信息
			updates := map[string]interface{}{
				"player_name": player.PlayerName,
				"player_age":  player.PlayerAge,
				"prefab":      player.Prefab,
				"last_seen":   now,
				"updated_at":  now,
			}

			// 如果状态从离线变为在线，更新状态和状态变更时间
			if existingPlayer.Status == PlayerStatusOffline {
				updates["status"] = PlayerStatusOnline
				updates["status_change"] = now
			}

			if err := tx.Model(&PlayerInfo{}).Where("id = ?", existingPlayer.ID).Updates(updates).Error; err != nil {
				tx.Rollback()
				return err
			}

			// 从映射中删除，剩下的将被标记为离线
			delete(existingPlayerMap, player.UserID)
		} else {
			// 新玩家，创建记录
			newPlayer := PlayerInfo{
				ArchiveName:  archiveName,
				UserID:       player.UserID,
				PlayerName:   player.PlayerName,
				PlayerAge:    player.PlayerAge,
				Prefab:       player.Prefab,
				Status:       PlayerStatusOnline,
				FirstSeen:    now,
				LastSeen:     now,
				StatusChange: now,
				CreatedAt:    now,
				UpdatedAt:    now,
			}

			if err := tx.Create(&newPlayer).Error; err != nil {
				tx.Rollback()
				return err
			}
		}
	}

	// 将不在当前在线列表中的玩家标记为离线
	for _, player := range existingPlayerMap {
		// 只更新当前在线的玩家为离线
		if player.Status == PlayerStatusOnline {
			updates := map[string]interface{}{
				"status":        PlayerStatusOffline,
				"status_change": now,
				"updated_at":    now,
			}

			if err := tx.Model(&PlayerInfo{}).Where("id = ?", player.ID).Updates(updates).Error; err != nil {
				tx.Rollback()
				return err
			}
		}
	}

	// 提交事务
	return tx.Commit().Error
}

// GetOnlinePlayers 获取在线玩家
// 如果提供 archiveName，则只获取指定存档的在线玩家
// 如果不提供 archiveName，则获取所有存档的在线玩家
func GetOnlinePlayers(archiveName string) ([]PlayerInfo, error) {
	var players []PlayerInfo
	query := db.Where("status = ?", PlayerStatusOnline)

	// 如果提供了存档名称，则添加到查询条件中
	if archiveName != "" {
		query = query.Where("archive_name = ?", archiveName)
	}

	// 执行查询
	err := query.Order("last_seen DESC").Find(&players).Error
	return players, err
}

// GetAllPlayers 获取玩家列表
// 如果提供 archiveName，则只获取指定存档的玩家
// 如果不提供 archiveName，则获取所有存档的玩家
func GetAllPlayers(archiveName string, page, pageSize int) ([]PlayerInfo, int, error) {
	var players []PlayerInfo
	var count int64

	// 构建查询
	query := db.Model(&PlayerInfo{})

	// 如果提供了存档名称，则添加到查询条件中
	if archiveName != "" {
		query = query.Where("archive_name = ?", archiveName)
	}

	// 获取总数
	if err := query.Count(&count).Error; err != nil {
		return nil, 0, err
	}

	// 分页查询
	offset := (page - 1) * pageSize
	if err := query.Order("last_seen DESC").Offset(offset).Limit(pageSize).Find(&players).Error; err != nil {
		return nil, 0, err
	}

	return players, int(count), nil
}

// GetPlayerByID 根据ID获取玩家信息
func GetPlayerByID(id int) (*PlayerInfo, error) {
	var player PlayerInfo
	err := db.Where("id = ?", id).First(&player).Error
	if err != nil {
		return nil, err
	}
	return &player, nil
}

// GetPlayerByUserID 根据UserID获取玩家信息
func GetPlayerByUserID(archiveName, userID string) (*PlayerInfo, error) {
	var player PlayerInfo
	err := db.Where("archive_name = ? AND user_id = ?", archiveName, userID).First(&player).Error
	if err != nil {
		return nil, err
	}
	return &player, nil
}

// GetPlayerStats 获取玩家统计信息
// 如果提供 archiveName，则只获取指定存档的统计信息
// 如果不提供 archiveName，则获取所有存档的统计信息
func GetPlayerStats(archiveName string) (map[string]interface{}, error) {
	var totalCount, onlineCount, offlineCount int64

	// 构建基本查询
	baseQuery := db.Model(&PlayerInfo{})
	if archiveName != "" {
		baseQuery = baseQuery.Where("archive_name = ?", archiveName)
	}

	// 获取总玩家数
	if err := baseQuery.Count(&totalCount).Error; err != nil {
		return nil, err
	}

	// 获取在线玩家数
	onlineQuery := baseQuery
	if err := onlineQuery.Where("status = ?", PlayerStatusOnline).Count(&onlineCount).Error; err != nil {
		return nil, err
	}

	// 获取离线玩家数
	offlineQuery := baseQuery
	if err := offlineQuery.Where("status = ?", PlayerStatusOffline).Count(&offlineCount).Error; err != nil {
		return nil, err
	}

	// 获取最近加入的玩家
	var recentPlayers []PlayerInfo
	recentQuery := db.Model(&PlayerInfo{})
	if archiveName != "" {
		recentQuery = recentQuery.Where("archive_name = ?", archiveName)
	}
	if err := recentQuery.Order("first_seen DESC").Limit(5).Find(&recentPlayers).Error; err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"total_count":    totalCount,
		"online_count":   onlineCount,
		"offline_count":  offlineCount,
		"recent_players": recentPlayers,
	}, nil
}

// SetAllPlayersOffline 将指定存档中的所有在线玩家状态更新为离线
func SetAllPlayersOffline(archiveName string) error {
	log.Printf("[PlayerInfo] 开始将存档 %s 中的所有在线玩家状态更新为离线", archiveName)

	// 获取当前时间
	now := time.Now()

	// 开启事务
	tx := db.Begin()
	if tx.Error != nil {
		return tx.Error
	}

	// 更新所有在线玩家的状态
	updates := map[string]interface{}{
		"status":        PlayerStatusOffline,
		"status_change": now,
		"updated_at":    now,
	}

	// 执行更新
	result := tx.Model(&PlayerInfo{}).Where("archive_name = ? AND status = ?", archiveName, PlayerStatusOnline).Updates(updates)
	if result.Error != nil {
		tx.Rollback()
		log.Printf("[PlayerInfo] 更新玩家状态失败: %v", result.Error)
		return result.Error
	}

	// 提交事务
	if err := tx.Commit().Error; err != nil {
		log.Printf("[PlayerInfo] 提交事务失败: %v", err)
		return err
	}

	log.Printf("[PlayerInfo] 成功将 %s 存档中的 %d 个在线玩家状态更新为离线", archiveName, result.RowsAffected)
	return nil
}

// GetPlayerArchives 获取玩家数据库中存在的所有存档列表
func GetPlayerArchives() ([]string, error) {
	log.Println("[PlayerInfo] 开始查询玩家数据库中的存档列表")

	// 查询所有不同的存档名称
	rows, err := db.Model(&PlayerInfo{}).Select("DISTINCT archive_name").Rows()
	if err != nil {
		log.Printf("[PlayerInfo] 查询存档列表失败: %v", err)
		return nil, err
	}
	defer rows.Close()

	// 提取存档名称
	var archives []string
	for rows.Next() {
		var archiveName string
		if err := rows.Scan(&archiveName); err != nil {
			log.Printf("[PlayerInfo] 扫描存档名称失败: %v", err)
			return nil, err
		}
		archives = append(archives, archiveName)
	}

	log.Printf("[PlayerInfo] 查询到 %d 个存档", len(archives))
	return archives, nil
}
