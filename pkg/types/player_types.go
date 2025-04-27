package types

// PlayerConfigInfo 玩家配置信息
type PlayerConfigInfo struct {
	UserID         string            // 玩家ID
	Name           string            // 玩家名称
	Admin          bool              // 是否管理员
	EventLevel     int               // 事件等级
	Muted          bool              // 是否被禁言
	Friend         bool              // 是否好友
	PlayerAge      int               // 玩家年龄
	IsHost         bool              // 是否为服务器主机
	UserFlags      int               // 用户标志
	Performance    int               // 性能指标
	Prefab         string            // 角色预制件名称
	LobbyCharacter string            // 大厅角色名称
	Colour         [4]float32        // 颜色 (RGBA)
	BaseSkin       string            // 基础皮肤
	NetID          string            // 网络服务标识符（通常是SteamID64）
	NetScore       int               // 网络评分
	SkillSelection []int             // 技能选择
	Vanity         map[string]string // 装饰性物品
	Equip          map[string]string // 装备物品
}
