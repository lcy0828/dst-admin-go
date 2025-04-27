package models

import (
	"dont/pkg/types"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
)

// PlayerStatus 玩家状态
const (
	PlayerStatusOnline  = "online"  // 在线
	PlayerStatusOffline = "offline" // 离线
)

// PlayerInfo 玩家信息表
type PlayerInfo struct {
	ID             int       `gorm:"primary_key" json:"id"`
	ArchiveName    string    `json:"archive_name"`    // 存档名称
	WorldName      string    `json:"world_name"`      // 世界名称
	UserID         string    `json:"user_id"`         // 玩家ID (KU_xxx格式)
	PlayerName     string    `json:"player_name"`     // 玩家名称
	PlayerAge      int       `json:"player_age"`      // 玩家年龄
	Prefab         string    `json:"prefab"`          // 玩家角色
	Status         string    `json:"status"`          // 玩家状态：online/offline
	IsAdmin        bool      `json:"is_admin"`        // 是否管理员
	IsHost         bool      `json:"is_host"`         // 是否主机
	IsMuted        bool      `json:"is_muted"`        // 是否被禁言
	IsFriend       bool      `json:"is_friend"`       // 是否好友
	EventLevel     int       `json:"event_level"`     // 事件等级
	UserFlags      int       `json:"user_flags"`      // 用户标志
	Performance    int       `json:"performance"`     // 性能指标
	LobbyCharacter string    `json:"lobby_character"` // 大厅角色
	BaseSkin       string    `json:"base_skin"`       // 基础皮肤
	NetID          string    `json:"net_id"`          // 网络服务标识符（通常是SteamID64）
	NetScore       int       `json:"net_score"`       // 网络评分
	ColourR        float32   `json:"colour_r"`        // 颜色R分量
	ColourG        float32   `json:"colour_g"`        // 颜色G分量
	ColourB        float32   `json:"colour_b"`        // 颜色B分量
	ColourA        float32   `json:"colour_a"`        // 颜色A分量
	SkillSelection string    `json:"skill_selection"` // 技能选择，以JSON字符串存储
	Vanity         string    `json:"vanity"`          // 装饰性物品，以JSON字符串存储
	Equip          string    `json:"equip"`           // 装备物品，以JSON字符串存储
	FirstSeen      time.Time `json:"first_seen"`      // 首次出现时间
	LastSeen       time.Time `json:"last_seen"`       // 最后出现时间
	StatusChange   time.Time `json:"status_change"`   // 状态变更时间
	CreatedAt      time.Time `json:"created_at"`      // 记录创建时间
	UpdatedAt      time.Time `json:"updated_at"`      // 记录更新时间
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
	// 为 archive_name 和 user_id 添加组合索引
	if err := db.Model(&PlayerInfo{}).AddIndex("idx_player_info_archive_userid", "archive_name", "user_id").Error; err != nil {
		log.Printf("[PlayerInfo] 添加索引 idx_player_info_archive_userid 失败: %v", err)
	} else {
		log.Println("[PlayerInfo] 添加索引 idx_player_info_archive_userid 成功")
	}

	// 为 archive_name 和 user_id 添加唯一索引
	if err := db.Model(&PlayerInfo{}).AddUniqueIndex("idx_player_info_archive_userid_unique", "archive_name", "user_id").Error; err != nil {
		log.Printf("[PlayerInfo] 添加唯一索引 idx_player_info_archive_userid_unique 失败: %v", err)
	} else {
		log.Println("[PlayerInfo] 添加唯一索引 idx_player_info_archive_userid_unique 成功")
	}

	// 为 status 添加索引
	if err := db.Model(&PlayerInfo{}).AddIndex("idx_player_info_status", "status").Error; err != nil {
		log.Printf("[PlayerInfo] 添加索引 idx_player_info_status 失败: %v", err)
	} else {
		log.Println("[PlayerInfo] 添加索引 idx_player_info_status 成功")
	}

	// 为 last_seen 添加索引
	if err := db.Model(&PlayerInfo{}).AddIndex("idx_player_info_last_seen", "last_seen").Error; err != nil {
		log.Printf("[PlayerInfo] 添加索引 idx_player_info_last_seen 失败: %v", err)
	} else {
		log.Println("[PlayerInfo] 添加索引 idx_player_info_last_seen 成功")
	}

	// 为 player_name 添加索引
	if err := db.Model(&PlayerInfo{}).AddIndex("idx_player_info_player_name", "player_name").Error; err != nil {
		log.Printf("[PlayerInfo] 添加索引 idx_player_info_player_name 失败: %v", err)
	} else {
		log.Println("[PlayerInfo] 添加索引 idx_player_info_player_name 成功")
	}

	// 为 net_id 添加索引
	if err := db.Model(&PlayerInfo{}).AddIndex("idx_player_info_net_id", "net_id").Error; err != nil {
		log.Printf("[PlayerInfo] 添加索引 idx_player_info_net_id 失败: %v", err)
	} else {
		log.Println("[PlayerInfo] 添加索引 idx_player_info_net_id 成功")
	}

	log.Println("[PlayerInfo] 玩家信息表初始化完成")
}

// ParsePlayerListLog 解析玩家列表日志
// 解析格式: [DST-ADMIN-GO] [Listplayers] [0] [000] [KU_HQp7BOVs] [[Host]] []
func ParsePlayerListLog(rawContent string) []PlayerInfo {
	log.Printf("[PlayerInfo] 开始解析玩家列表日志")

	// 使用map来记录已经解析到的玩家，避免重复
	playerMap := make(map[string]PlayerInfo)

	// 分行处理
	lines := strings.Split(rawContent, "\n")
	log.Printf("[PlayerInfo] 分行后共有 %d 行", len(lines))

	// 正则表达式匹配玩家信息行
	// 格式1: [时间戳]: [DST-ADMIN-GO] [Listplayers] [index] [age] [userid] [name] [prefab]
	// 例如: [01:46:15]: [DST-ADMIN-GO] [Listplayers] [0] [000] [KU_HQp7BOVs] [[Host]] []
	re1 := regexp.MustCompile(`\[\d+:\d+:\d+\]: \[DST-ADMIN-GO\] \[Listplayers\] \[(\d+)\] \[(\d+)\] \[([^\]]+)\] \[([^\]]+)\] \[([^\]]*)\]`)

	// 格式2: [DST-ADMIN-GO] [Listplayers] [index] [age] [userid] [name] [prefab]
	// 例如: [DST-ADMIN-GO] [Listplayers] [0] [000] [KU_HQp7BOVs] [[Host]] []
	re2 := regexp.MustCompile(`\[DST-ADMIN-GO\] \[Listplayers\] \[(\d+)\] \[(\d+)\] \[([^\]]+)\] \[([^\]]+)\] \[([^\]]*)\]`)

	for i, line := range lines {
		log.Printf("[PlayerInfo] 处理第 %d 行: %s", i+1, line)

		// 先尝试格式1
		matches := re1.FindStringSubmatch(line)

		// 如果格式1不匹配，尝试格式2
		if len(matches) < 6 {
			matches = re2.FindStringSubmatch(line)
		}

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

			// 处理主机行，主机行也包含有用的信息
			// 如果是主机行，我们仍然记录它，因为它可能是服务器管理员
			// 但是我们会在日志中标记它
			if playerName == "[Host]" {
				log.Printf("[PlayerInfo] 检测到主机行，作为特殊玩家处理")
				// 不跳过，继续处理
				// 为主机行添加特殊标记
				playerName = "[Host-Admin]"
			}

			// 创建玩家信息对象
			player := PlayerInfo{
				UserID:     userID,
				PlayerName: playerName,
				PlayerAge:  age,
				Prefab:     prefab,
				Status:     PlayerStatusOnline,
			}

			// 检查玩家是否已经存在，如果存在则跳过
			if _, exists := playerMap[userID]; !exists {
				log.Printf("[PlayerInfo] 添加玩家: %s (%s)", player.PlayerName, player.UserID)
				playerMap[userID] = player
			} else {
				log.Printf("[PlayerInfo] 跳过重复玩家: %s (%s)", player.PlayerName, player.UserID)
			}
		} else {
			log.Printf("[PlayerInfo] 未匹配到玩家信息行: %s", line)
		}
	}

	// 将map转换为切片
	var players []PlayerInfo
	for _, player := range playerMap {
		players = append(players, player)
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

	// 即使没有解析到玩家信息，也继续执行，将所有玩家标记为离线
	if len(currentPlayers) == 0 {
		log.Printf("[PlayerInfo] 没有解析到玩家信息，将所有玩家标记为离线")
		// 不返回，继续执行，将所有玩家标记为离线
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
		// 如果已经存在该UserID，保留最新的一条记录
		if existingPlayer, exists := existingPlayerMap[player.UserID]; exists {
			// 如果当前玩家的最后见到时间更新，则替换
			if player.LastSeen.After(existingPlayer.LastSeen) {
				existingPlayerMap[player.UserID] = player
			}
		} else {
			existingPlayerMap[player.UserID] = player
		}
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

// GetPlayerConfigInfo 获取玩家配置信息
func GetPlayerConfigInfo(archiveName, worldName string, filterMode int) ([]PlayerInfo, error) {
	log.Printf("[PlayerInfo] 开始获取玩家配置信息, 存档: %s, 世界: %s, 过滤模式: %d",
		archiveName, worldName, filterMode)

	// 构建查询
	query := db.Model(&PlayerInfo{})

	// 添加存档名称条件
	query = query.Where("archive_name = ?", archiveName)

	// 如果提供了世界名称，添加世界名称条件
	if worldName != "" {
		query = query.Where("world_name = ?", worldName)
	}

	// 根据过滤模式添加条件
	switch filterMode {
	case 1: // 只显示主机
		query = query.Where("is_host = ?", true)
	case 2: // 只显示玩家
		query = query.Where("is_host = ?", false)
	}

	// 执行查询
	var players []PlayerInfo
	err := query.Order("last_seen DESC").Find(&players).Error
	if err != nil {
		log.Printf("[PlayerInfo] 获取玩家配置信息失败: %v", err)
		return nil, err
	}

	log.Printf("[PlayerInfo] 成功获取 %d 个玩家配置信息", len(players))
	return players, nil
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

// DeleteDuplicatePlayerInfo 删除重复的玩家信息记录
// 根据archive_name和user_id判断唯一性，保留最新的记录
func DeleteDuplicatePlayerInfo() error {
	log.Println("[PlayerInfo] 开始删除重复的玩家信息记录")

	// 查询所有玩家信息记录，按archive_name和user_id分组
	rows, err := db.Raw(`
		SELECT p1.id, p1.archive_name, p1.user_id
		FROM dont_player_info p1
		INNER JOIN (
			SELECT archive_name, user_id, MAX(id) as max_id
			FROM dont_player_info
			GROUP BY archive_name, user_id
			HAVING COUNT(*) > 1
		) p2 ON p1.archive_name = p2.archive_name AND p1.user_id = p2.user_id AND p1.id != p2.max_id
	`).Rows()

	if err != nil {
		log.Printf("[PlayerInfo] 查询重复记录失败: %v", err)
		return err
	}
	defer rows.Close()

	// 删除重复记录
	var duplicateIDs []int
	for rows.Next() {
		var id int
		var archiveName, userID string
		if err := rows.Scan(&id, &archiveName, &userID); err != nil {
			log.Printf("[PlayerInfo] 扫描行数据失败: %v", err)
			continue
		}
		duplicateIDs = append(duplicateIDs, id)
		log.Printf("[PlayerInfo] 发现重复记录: ID=%d, 存档=%s, 用户ID=%s", id, archiveName, userID)
	}

	// 删除重复记录
	if len(duplicateIDs) > 0 {
		for _, id := range duplicateIDs {
			if err := db.Where("id = ?", id).Delete(&PlayerInfo{}).Error; err != nil {
				log.Printf("[PlayerInfo] 删除重复记录失败: ID=%d, 错误=%v", id, err)
			} else {
				log.Printf("[PlayerInfo] 成功删除重复记录: ID=%d", id)
			}
		}
	} else {
		log.Println("[PlayerInfo] 未发现重复记录")
	}

	return nil
}

// RebuildPlayerInfoTable 重建玩家信息表结构
// 这个函数会删除旧的唯一索引，并添加新的唯一索引
func RebuildPlayerInfoTable() error {
	log.Println("[PlayerInfo] 开始重建玩家信息表结构")

	// 删除旧的唯一索引
	if err := db.Exec("DROP INDEX IF EXISTS idx_player_info_archive_world_userid").Error; err != nil {
		log.Printf("[PlayerInfo] 删除旧的唯一索引失败: %v", err)
		return err
	}
	log.Println("[PlayerInfo] 成功删除旧的唯一索引")

	// 添加新的唯一索引
	if err := db.Model(&PlayerInfo{}).AddUniqueIndex("idx_player_info_archive_userid_unique", "archive_name", "user_id").Error; err != nil {
		log.Printf("[PlayerInfo] 添加新的唯一索引失败: %v", err)
		return err
	}
	log.Println("[PlayerInfo] 成功添加新的唯一索引")

	// 删除重复记录
	if err := DeleteDuplicatePlayerInfo(); err != nil {
		log.Printf("[PlayerInfo] 删除重复记录失败: %v", err)
		return err
	}

	log.Println("[PlayerInfo] 玩家信息表结构重建完成")
	return nil
}

// SavePlayerConfigInfo 保存玩家配置信息到数据库
func SavePlayerConfigInfo(archiveName, worldName string, playerConfigs []types.PlayerConfigInfo) error {
	log.Printf("[PlayerInfo] 开始保存玩家配置信息到数据库, 存档: %s, 世界: %s, 玩家数: %d",
		archiveName, worldName, len(playerConfigs))

	// 获取当前时间
	now := time.Now()

	// 开启事务
	tx := db.Begin()
	if tx.Error != nil {
		return tx.Error
	}

	// 创建一个map来跟踪配置文件中的玩家ID
	currentPlayerIDs := make(map[string]bool)
	for _, config := range playerConfigs {
		currentPlayerIDs[config.UserID] = true
	}

	// 检查是否只有主机，没有其他玩家
	onlyHostExists := false
	if len(playerConfigs) == 1 {
		// 检查唯一的玩家是否是主机
		if playerConfigs[0].IsHost {
			log.Printf("[PlayerInfo] 检测到存档 %s 世界 %s 中只有主机，没有其他玩家", archiveName, worldName)
			onlyHostExists = true
		}
	}

	// 处理每个玩家配置
	for _, config := range playerConfigs {
		// 如果是主机，则保存到主机信息表
		if config.IsHost {
			// 将技能选择、装饰物品和装备物品转换为JSON字符串
			skillSelectionJSON, _ := json.Marshal(config.SkillSelection)
			vanityJSON, _ := json.Marshal(config.Vanity)
			equipJSON, _ := json.Marshal(config.Equip)

			// 创建主机信息对象
			hostInfo := &HostInfo{
				ArchiveName:    archiveName,
				WorldName:      worldName,
				UserID:         config.UserID,
				Name:           config.Name,
				Performance:    config.Performance,
				EventLevel:     config.EventLevel,
				UserFlags:      config.UserFlags,
				ColourR:        config.Colour[0],
				ColourG:        config.Colour[1],
				ColourB:        config.Colour[2],
				ColourA:        config.Colour[3],
				SkillSelection: string(skillSelectionJSON),
				Vanity:         string(vanityJSON),
				Equip:          string(equipJSON),
				LastSeen:       now,
				UpdatedAt:      now,
			}

			// 同时检查主机的UserID是否在PlayerInfo表中存在
			// 如果存在，则将其标记为离线
			var hostPlayer PlayerInfo
			hostResult := tx.Where("archive_name = ? AND user_id = ?", archiveName, config.UserID).First(&hostPlayer)
			if hostResult.Error == nil {
				// 如果主机在PlayerInfo表中存在，则将其标记为离线
				updates := map[string]interface{}{
					"status":        PlayerStatusOffline,
					"status_change": now,
					"updated_at":    now,
				}
				if err := tx.Model(&PlayerInfo{}).Where("id = ?", hostPlayer.ID).Updates(updates).Error; err != nil {
					log.Printf("[PlayerInfo] 更新主机在PlayerInfo表中的状态失败: %v", err)
				} else {
					log.Printf("[PlayerInfo] 主机状态从在线变为离线: UserID=%s, 名称='%s', 存档=%s",
						config.UserID, config.Name, archiveName)
				}
			}

			// 保存主机信息 - 根据存档名称和用户ID判断唯一性
			var existingHost HostInfo
			result := tx.Where("archive_name = ? AND user_id = ?",
				archiveName, config.UserID).First(&existingHost)

			if result.Error != nil && !gorm.IsRecordNotFoundError(result.Error) {
				// 查询出错，但不是因为记录不存在
				tx.Rollback()
				return result.Error
			} else if gorm.IsRecordNotFoundError(result.Error) {
				// 主机不存在，创建新记录
				hostInfo.FirstSeen = now
				hostInfo.CreatedAt = now

				if err := tx.Create(hostInfo).Error; err != nil {
					tx.Rollback()
					return err
				}

				log.Printf("[PlayerInfo] 创建新主机记录: UserID=%s, 名称='%s', 存档=%s, 世界=%s",
					config.UserID, config.Name, archiveName, worldName)
			} else {
				// 主机已存在，更新记录
				existingHost.Name = config.Name
				existingHost.Performance = config.Performance
				existingHost.EventLevel = config.EventLevel
				existingHost.UserFlags = config.UserFlags
				existingHost.ColourR = config.Colour[0]
				existingHost.ColourG = config.Colour[1]
				existingHost.ColourB = config.Colour[2]
				existingHost.ColourA = config.Colour[3]
				existingHost.SkillSelection = string(skillSelectionJSON)
				existingHost.Vanity = string(vanityJSON)
				existingHost.Equip = string(equipJSON)
				existingHost.LastSeen = now
				existingHost.UpdatedAt = now

				if err := tx.Save(&existingHost).Error; err != nil {
					tx.Rollback()
					return err
				}

				log.Printf("[PlayerInfo] 更新主机记录: UserID=%s, 名称='%s', 存档=%s, 世界=%s",
					config.UserID, config.Name, archiveName, worldName)
			}

			// 主机信息已处理，继续下一个配置
			continue
		}

		// 如果不是主机，则保存到玩家信息表
		// 查询玩家是否已存在 - 根据存档名称和用户ID判断唯一性
		var existingPlayer PlayerInfo
		result := tx.Where("archive_name = ? AND user_id = ?",
			archiveName, config.UserID).First(&existingPlayer)

		// 将技能选择、装饰物品和装备物品转换为JSON字符串
		skillSelectionJSON, _ := json.Marshal(config.SkillSelection)
		vanityJSON, _ := json.Marshal(config.Vanity)
		equipJSON, _ := json.Marshal(config.Equip)

		if result.Error != nil && !gorm.IsRecordNotFoundError(result.Error) {
			// 查询出错，但不是因为记录不存在
			tx.Rollback()
			return result.Error
		} else if gorm.IsRecordNotFoundError(result.Error) {
			// 玩家不存在，创建新记录
			newPlayer := PlayerInfo{
				ArchiveName:    archiveName,
				WorldName:      worldName,
				UserID:         config.UserID,
				PlayerName:     config.Name,
				PlayerAge:      config.PlayerAge,
				Prefab:         config.Prefab,
				Status:         PlayerStatusOnline, // 设置为在线状态，因为玩家配置文件中的玩家都是在线的
				IsAdmin:        config.Admin,
				IsHost:         config.IsHost,
				IsMuted:        config.Muted,
				IsFriend:       config.Friend,
				EventLevel:     config.EventLevel,
				UserFlags:      config.UserFlags,
				Performance:    config.Performance,
				LobbyCharacter: config.LobbyCharacter,
				BaseSkin:       config.BaseSkin,
				NetID:          config.NetID,
				NetScore:       config.NetScore,
				ColourR:        config.Colour[0],
				ColourG:        config.Colour[1],
				ColourB:        config.Colour[2],
				ColourA:        config.Colour[3],
				SkillSelection: string(skillSelectionJSON),
				Vanity:         string(vanityJSON),
				Equip:          string(equipJSON),
				FirstSeen:      now,
				LastSeen:       now,
				StatusChange:   now,
				CreatedAt:      now,
				UpdatedAt:      now,
			}

			if err := tx.Create(&newPlayer).Error; err != nil {
				tx.Rollback()
				return err
			}

			log.Printf("[PlayerInfo] 创建新玩家记录: UserID=%s, 名称='%s', 存档=%s, 世界=%s",
				config.UserID, config.Name, archiveName, worldName)
		} else {
			// 玩家已存在，更新记录
			existingPlayer.PlayerName = config.Name
			existingPlayer.PlayerAge = config.PlayerAge
			existingPlayer.Prefab = config.Prefab
			existingPlayer.IsAdmin = config.Admin
			existingPlayer.IsHost = config.IsHost
			existingPlayer.IsMuted = config.Muted
			existingPlayer.IsFriend = config.Friend
			existingPlayer.EventLevel = config.EventLevel
			existingPlayer.UserFlags = config.UserFlags
			existingPlayer.Performance = config.Performance
			existingPlayer.LobbyCharacter = config.LobbyCharacter
			existingPlayer.BaseSkin = config.BaseSkin
			existingPlayer.NetID = config.NetID
			existingPlayer.NetScore = config.NetScore
			existingPlayer.ColourR = config.Colour[0]
			existingPlayer.ColourG = config.Colour[1]
			existingPlayer.ColourB = config.Colour[2]
			existingPlayer.ColourA = config.Colour[3]
			existingPlayer.SkillSelection = string(skillSelectionJSON)
			existingPlayer.Vanity = string(vanityJSON)
			existingPlayer.Equip = string(equipJSON)
			existingPlayer.LastSeen = now
			existingPlayer.UpdatedAt = now

			// 更新玩家状态，如果玩家当前为离线状态，则更新为在线状态
			if existingPlayer.Status == PlayerStatusOffline {
				existingPlayer.Status = PlayerStatusOnline
				existingPlayer.StatusChange = now
				log.Printf("[PlayerInfo] 玩家状态从离线变为在线: UserID=%s, 名称='%s', 存档=%s",
					config.UserID, config.Name, archiveName)
			}

			if err := tx.Save(&existingPlayer).Error; err != nil {
				tx.Rollback()
				return err
			}

			log.Printf("[PlayerInfo] 更新玩家记录: UserID=%s, 名称='%s', 存档=%s, 世界=%s",
				config.UserID, config.Name, archiveName, worldName)
		}
	}

	// 将数据库中存在但配置文件中不存在的玩家标记为离线
	var existingPlayers []PlayerInfo

	// 如果只有主机，没有其他玩家，则获取所有在线玩家
	if onlyHostExists {
		// 获取存档中所有在线的玩家（包括主机和非主机）
		if err := tx.Where("archive_name = ? AND status = ?", archiveName, PlayerStatusOnline).Find(&existingPlayers).Error; err != nil {
			tx.Rollback()
			return fmt.Errorf("获取存档 %s 中的在线玩家失败: %v", archiveName, err)
		}

		// 如果有在线玩家，则将它们全部标记为离线
		if len(existingPlayers) > 0 {
			log.Printf("[PlayerInfo] 存档 %s 世界 %s 中只有主机，将 %d 个在线玩家标记为离线",
				archiveName, worldName, len(existingPlayers))

			// 批量更新所有在线玩家为离线状态
			updates := map[string]interface{}{
				"status":        PlayerStatusOffline,
				"status_change": now,
				"updated_at":    now,
			}

			if err := tx.Model(&PlayerInfo{}).Where("archive_name = ? AND status = ?",
				archiveName, PlayerStatusOnline).Updates(updates).Error; err != nil {
				tx.Rollback()
				return fmt.Errorf("批量更新存档 %s 中的玩家状态失败: %v", archiveName, err)
			}

			// 输出每个玩家的状态变化日志
			for _, player := range existingPlayers {
				log.Printf("[PlayerInfo] 玩家状态从在线变为离线: UserID=%s, 名称='%s', 存档=%s",
					player.UserID, player.PlayerName, archiveName)
			}
		}
	} else {
		// 正常情况，获取所有在线玩家
		if err := tx.Where("archive_name = ? AND status = ?", archiveName, PlayerStatusOnline).Find(&existingPlayers).Error; err != nil {
			tx.Rollback()
			return fmt.Errorf("获取存档 %s 中的在线玩家失败: %v", archiveName, err)
		}

		// 检查每个在线玩家，如果不在当前配置文件中，则标记为离线
		for _, player := range existingPlayers {
			if !currentPlayerIDs[player.UserID] {
				// 玩家不在当前配置文件中，标记为离线
				updates := map[string]interface{}{
					"status":        PlayerStatusOffline,
					"status_change": now,
					"updated_at":    now,
				}

				if err := tx.Model(&PlayerInfo{}).Where("id = ?", player.ID).Updates(updates).Error; err != nil {
					tx.Rollback()
					return fmt.Errorf("更新玩家 %s (%s) 的状态失败: %v", player.PlayerName, player.UserID, err)
				}

				log.Printf("[PlayerInfo] 玩家状态从在线变为离线: UserID=%s, 名称='%s', 存档=%s",
					player.UserID, player.PlayerName, archiveName)
			}
		}
	}

	// 提交事务
	if err := tx.Commit().Error; err != nil {
		return err
	}

	log.Printf("[PlayerInfo] 成功保存 %d 个玩家配置信息到数据库", len(playerConfigs))
	return nil
}
