package dstserver

import (
	"dont/pkg/configpath"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/go-ini/ini"
)

// 配置变量
var (
	dstServerPath string // DST服务器安装路径
)

// 初始化函数，从配置文件读取配置
func init() {
	// 默认配置
	dstServerPath = "./dstserver"

	// 尝试从配置文件读取
	configFile := configpath.Current()
	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		if cfg, err := ini.Load(configFile); err == nil {
			// 读取路径配置
			if cfg.Section("paths").HasKey("DST_SERVER_PATH") {
				dstServerPath = cfg.Section("paths").Key("DST_SERVER_PATH").String()
				log.Printf("从配置文件加载DST服务器安装路径: %s", dstServerPath)
			}
		}
	} else {
		log.Printf("配置文件不存在，使用默认DST服务器安装路径: %s", dstServerPath)
	}
}

// LocalVersionInfo 本地版本信息结构
type LocalVersionInfo struct {
	Version     string `json:"version"`      // 版本号
	InstallPath string `json:"install_path"` // 安装路径
}

// GetLocalDSTVersion 获取本地安装的饥荒服务端版本
func GetLocalDSTVersion(c *gin.Context) {
	clientIP := c.ClientIP()
	log.Printf("[API][GetLocalDSTVersion] 收到获取本地版本请求 IP: %s", clientIP)

	// 构建version.txt文件路径
	versionFilePath := filepath.Join(dstServerPath, "version.txt")

	// 检查文件是否存在
	if _, err := os.Stat(versionFilePath); os.IsNotExist(err) {
		log.Printf("[API][GetLocalDSTVersion] 版本文件不存在: %s", versionFilePath)
		c.JSON(http.StatusNotFound, gin.H{
			"status": 404,
			"msg":    fmt.Sprintf("版本文件不存在: %s", versionFilePath),
		})
		return
	}

	// 读取版本文件内容
	versionBytes, err := ioutil.ReadFile(versionFilePath)
	if err != nil {
		log.Printf("[API][GetLocalDSTVersion] 读取版本文件失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("读取版本文件失败: %v", err),
		})
		return
	}

	// 解析版本号
	version := strings.TrimSpace(string(versionBytes))

	// 返回版本信息
	versionInfo := LocalVersionInfo{
		Version:     version,
		InstallPath: dstServerPath,
	}

	log.Printf("[API][GetLocalDSTVersion] 成功获取本地版本: %s, 安装路径: %s", version, dstServerPath)

	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取本地版本成功",
		"data":   versionInfo,
	})
}
