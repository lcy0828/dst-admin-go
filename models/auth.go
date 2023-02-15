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
type Mod_version struct {
	ID              int         `gorm:"primary_key" json:"id"`
	Modid           string      `json:"modid"`
	Version         string      `json:"version"`
	Last_updatetime *times.Time `json:"time"`
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
func Updatemodversion(modid string, version string) bool {
	//var mod_version Mod_version
	db.AutoMigrate(&Mod_version{})
	time := times.Now()
	var mod_version = Mod_version{Modid: modid, Version: version, Last_updatetime: &time}
	//db.Create(&mod_version)
	if err := db.Create(&mod_version).Error; err != nil {
		fmt.Println("插入modversion失败", err)
		return false
	}
	return true
}

//func Checkmodversion(modid, version string) bool {
//	var mod_version Mod_version
//	db.Select("id").Where(Auth{Username: username, Password: password}).First(&mod_version)
//	if auth.ID > 0 {
//		return true
//	}
//	return false
//}
