package models

import (
	"dont/pkg/setting"
	"fmt"
	"log"

	"github.com/jinzhu/gorm"
	//_ "github.com/jinzhu/gorm/dialects/mysql"
	_ "github.com/mattn/go-sqlite3"
)

var db *gorm.DB

type Model struct {
	ID         int `gorm:"primary_key" json:"id"`
	CreatedOn  int `json:"created_on"`
	ModifiedOn int `json:"modified_on"`
}

func init() {
	//var (
	//	err                                               error
	//	dbType, dbName, user, password, host, tablePrefix string
	//)
	var (
		err                       error
		dbType, path, tablePrefix string
	)
	fmt.Println("sqllite3.....")
	sec, err := setting.Cfg.GetSection("database")
	if err != nil {
		log.Fatal(2, "Fail to get section 'database': %v", err)
	}

	//dbType = sec.Key("TYPE").String()
	//dbName = sec.Key("NAME").String()
	//user = sec.Key("USER").String()
	//password = sec.Key("PASSWORD").String()
	//host = sec.Key("HOST").String()
	tablePrefix = sec.Key("TABLE_PREFIX").String()
	dbType = sec.Key("TYPE").String()
	path = sec.Key("PATH").String()

	//db, err = gorm.Open(dbType, fmt.Sprintf("%s:%s@tcp(%s)/%s?charset=utf8&parseTime=True&loc=Local",
	//	user,
	//	password,
	//	host,
	//	dbName))
	db, err = gorm.Open(dbType, path)
	fmt.Println(dbType, path)
	if err != nil {
		log.Println(err)
	}

	gorm.DefaultTableNameHandler = func(db *gorm.DB, defaultTableName string) string {
		return tablePrefix + defaultTableName
	}

	db.SingularTable(true)
	db.LogMode(true)
	db.DB().SetMaxIdleConns(10)
	db.DB().SetMaxOpenConns(100)

	// 初始化游戏日志相关表结构
	initGameLogTables()

	// 初始化定时任务相关表结构
	InitCronTables()
}

func CloseDB() {
	defer db.Close()
}

// DB 返回数据库连接
func DB() *gorm.DB {
	return db
}

// 初始化游戏日志相关表结构
func initGameLogTables() {
	// 自动迁移表结构
	db.AutoMigrate(&GameLog{})
	db.AutoMigrate(&LogExtractRule{})
	db.AutoMigrate(&LogStatistics{})
	// 初始化玩家信息表
	InitPlayerInfoTable()
	// 初始化主机信息表
	InitHostInfoTable()
	// 初始化世界状态信息表
	InitWorldStateInfoTable()

	log.Println("游戏日志相关表结构初始化完成")
}
