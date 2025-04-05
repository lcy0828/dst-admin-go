package models

import (
	"fmt"
	times "time"
)

type Auth struct {
	ID       int    `gorm:"primary_key" json:"id"`
	Username string `json:"username"`
	Password string `json:"password"`
}
type Login_history struct {
	ID       int         `gorm:"primary_key" json:"id"`
	Username string      `json:"username"`
	Time     *times.Time `json:"time"`
}

// Mod_version结构体已被移除，使用Mod_info替代

type Mod_info struct {
	ID              int         `gorm:"primary_key" json:"id"`
	Modid           string      `json:"modid"`
	Name            string      `json:"name"`
	Author          string      `json:"author"`
	Version         string      `json:"version"`
	Description     string      `json:"description"`
	Tags            string      `json:"tags"`
	Subscribers     int         `json:"subscribers"`
	Config          string      `gorm:"type:text" json:"config"` // 存储完整的JSON配置信息
	Last_updatetime *times.Time `json:"time"`
}

// Server_mod 服务器模组信息
type Server_mod struct {
	ID              int         `gorm:"primary_key" json:"id"`
	Modid           string      `json:"modid"`
	Name            string      `json:"name"`
	Author          string      `json:"author"`
	Version         string      `json:"version"`
	Image           string      `json:"image"`
	Subscribers     string      `json:"subscribers"`
	UpdateTime      string      `json:"update_time"`
	Rating          string      `json:"rating"`
	Enabled         bool        `json:"enabled" gorm:"default:true"`
	Last_updatetime *times.Time `json:"time"`
}

// Mod_config 模组自定义配置
type Mod_config struct {
	ID                   int         `gorm:"primary_key" json:"id"`
	Modid                string      `json:"modid"`
	ConfigurationOptions string      `gorm:"type:text" json:"configuration_options"`
	Enabled              bool        `json:"enabled" gorm:"default:true"`
	Last_updatetime      *times.Time `json:"time"`
}

func CheckAuth(username, password string) bool {
	var auth Auth
	db.Select("id").Where(Auth{Username: username, Password: password}).First(&auth)
	if auth.ID > 0 {
		return true
	}
	return false
}

func ChangePasswd(username, password1, password2 string) bool {
	var auth Auth
	result := db.Where(Auth{Username: username, Password: password1}).First(&auth).Update(Auth{Username: username, Password: password2})
	if result.Error != nil {
		return false
	}
	//fmt.Print(result.RowsAffected)
	return true
}

func Login_sent(username string) bool {
	//var login_history Login_history
	time := times.Now()
	db.AutoMigrate(&Login_history{})
	var login_history = Login_history{Username: username, Time: &time}
	db.Create(&login_history)
	if err := db.Create(&login_history).Error; err != nil {
		//fmt.Println("插入失败", err)
		return true
	}
	return true
}

// Updatemodversion函数已被移除，使用UpdateModInfo替代

// UpdateModInfo 更新模组完整信息
func UpdateModInfo(modid, name, author, version, description, tags string, subscribers int, config string) bool {
	// 自动迁移确保表存在
	db.AutoMigrate(&Mod_info{})

	// 获取当前时间
	time := times.Now()

	// 检查是否已存在该模组记录
	var existingMod Mod_info
	result := db.Where("modid = ?", modid).First(&existingMod)

	// 如果模组已存在，则更新记录
	if result.Error == nil {
		updates := map[string]interface{}{
			"name":            name,
			"author":          author,
			"version":         version,
			"description":     description,
			"tags":            tags,
			"subscribers":     subscribers,
			"config":          config,
			"last_updatetime": &time,
		}

		if err := db.Model(&existingMod).Updates(updates).Error; err != nil {
			fmt.Println("更新mod_info失败:", err)
			return false
		}
		fmt.Printf("更新模组信息成功 - 模组ID: %s, 名称: %s\n", modid, name)
		return true
	}

	// 如果模组不存在，则创建新记录
	var modInfo = Mod_info{
		Modid:           modid,
		Name:            name,
		Author:          author,
		Version:         version,
		Description:     description,
		Tags:            tags,
		Subscribers:     subscribers,
		Config:          config,
		Last_updatetime: &time,
	}

	if err := db.Create(&modInfo).Error; err != nil {
		fmt.Println("插入mod_info失败:", err)
		return false
	}

	fmt.Printf("创建模组信息成功 - 模组ID: %s, 名称: %s\n", modid, name)
	return true
}

// AddServerMod 添加或更新服务器模组信息
func AddServerMod(modid, name, author, version, image, subscribers, updateTime, rating, config string) bool {
	// 自动迁移确保表存在
	db.AutoMigrate(&Server_mod{})

	// 获取当前时间
	time := times.Now()

	// 检查是否已存在该模组记录
	var existingMod Server_mod
	result := db.Where("modid = ?", modid).First(&existingMod)

	// 如果模组已存在，则更新记录
	if result.Error == nil {
		updates := map[string]interface{}{
			"name":            name,
			"author":          author,
			"version":         version,
			"image":           image,
			"subscribers":     subscribers,
			"update_time":     updateTime,
			"rating":          rating,
			"config":          config,
			"last_updatetime": &time,
		}

		if err := db.Model(&existingMod).Updates(updates).Error; err != nil {
			fmt.Println("更新server_mod失败:", err)
			return false
		}
		fmt.Printf("更新服务器模组信息成功 - 模组ID: %s, 名称: %s\n", modid, name)
		return true
	}

	// 如果模组不存在，则创建新记录
	var serverMod = Server_mod{
		Modid:           modid,
		Name:            name,
		Author:          author,
		Version:         version,
		Image:           image,
		Subscribers:     subscribers,
		UpdateTime:      updateTime,
		Rating:          rating,
		Enabled:         true, // 默认为启用状态
		Last_updatetime: &time,
	}

	if err := db.Create(&serverMod).Error; err != nil {
		fmt.Println("插入server_mod失败:", err)
		return false
	}

	fmt.Printf("创建服务器模组信息成功 - 模组ID: %s, 名称: %s\n", modid, name)
	return true
}

// GetServerMods 获取所有服务器模组信息
func GetServerMods() ([]Server_mod, error) {
	var mods []Server_mod
	result := db.Find(&mods)
	if result.Error != nil {
		return nil, result.Error
	}
	return mods, nil
}

// GetModConfig 获取模组的配置信息
func GetModConfig(modid string) (string, error) {
	var mod Mod_info
	result := db.Where("modid = ?", modid).First(&mod)
	if result.Error != nil {
		return "", result.Error
	}
	return mod.Config, nil
}

// SaveModCustomConfig 保存模组自定义配置
func SaveModCustomConfig(modid string, configOptions string, enabled bool) (bool, error) {
	// 自动迁移确保表存在
	db.AutoMigrate(&Mod_config{})

	// 获取当前时间
	time := times.Now()

	// 检查是否已存在该模组配置
	var existingConfig Mod_config
	result := db.Where("modid = ?", modid).First(&existingConfig)

	// 如果模组配置已存在，则更新记录
	if result.Error == nil {
		updates := map[string]interface{}{
			"configuration_options": configOptions,
			"enabled":               enabled,
			"last_updatetime":       &time,
		}

		if err := db.Model(&existingConfig).Updates(updates).Error; err != nil {
			fmt.Println("更新mod_config失败:", err)
			return false, err
		}
		fmt.Printf("更新模组自定义配置成功 - 模组ID: %s\n", modid)
		return true, nil
	}

	// 如果模组配置不存在，则创建新记录
	var modConfig = Mod_config{
		Modid:                modid,
		ConfigurationOptions: configOptions,
		Enabled:              enabled,
		Last_updatetime:      &time,
	}

	if err := db.Create(&modConfig).Error; err != nil {
		fmt.Println("插入mod_config失败:", err)
		return false, err
	}

	fmt.Printf("创建模组自定义配置成功 - 模组ID: %s\n", modid)
	return true, nil
}

// GetModCustomConfig 获取模组自定义配置
func GetModCustomConfig(modid string) (*Mod_config, error) {
	var modConfig Mod_config
	result := db.Where("modid = ?", modid).First(&modConfig)
	if result.Error != nil {
		return nil, result.Error
	}
	return &modConfig, nil
}

// GetAllModConfigs 获取所有模组的自定义配置
func GetAllModConfigs() ([]Mod_config, error) {
	var configs []Mod_config
	result := db.Find(&configs)
	if result.Error != nil {
		return nil, result.Error
	}
	return configs, nil
}

// DeleteServerMod 从服务器删除模组
func DeleteServerMod(modid string) (bool, error) {
	// 删除服务器模组记录
	result := db.Where("modid = ?", modid).Delete(&Server_mod{})
	if result.Error != nil {
		return false, result.Error
	}

	// 删除模组自定义配置
	configResult := db.Where("modid = ?", modid).Delete(&Mod_config{})
	if configResult.Error != nil {
		fmt.Printf("删除模组自定义配置失败 - 模组ID: %s, 错误: %v\n", modid, configResult.Error)
		// 即使配置删除失败，也认为模组删除成功
	}

	return result.RowsAffected > 0, nil
}

// UpdateModEnabled 更新模组启用状态
func UpdateModEnabled(modid string, enabled bool) (bool, error) {
	// 更新服务器模组表中的启用状态
	time := times.Now()

	// 检查是否已存在该模组
	var existingMod Server_mod
	result := db.Where("modid = ?", modid).First(&existingMod)

	// 如果模组已存在，则更新启用状态
	if result.Error == nil {
		updates := map[string]interface{}{
			"enabled":         enabled,
			"last_updatetime": &time,
		}

		if err := db.Model(&existingMod).Updates(updates).Error; err != nil {
			fmt.Printf("更新模组启用状态失败 - 模组ID: %s, 错误: %v\n", modid, err)
			return false, err
		}

		fmt.Printf("更新模组启用状态成功 - 模组ID: %s, 启用状态: %v\n", modid, enabled)
		return true, nil
	}

	// 如果模组不存在，返回错误
	fmt.Printf("模组不存在，无法更新启用状态 - 模组ID: %s\n", modid)
	return false, fmt.Errorf("模组不存在: %s", modid)
}
