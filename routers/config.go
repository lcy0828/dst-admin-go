package routers

import (
	"github.com/go-ini/ini"
)

// LoadConfig 加载配置文件
func LoadConfig(configFile string) (*ini.File, error) {
	return ini.Load(configFile)
}
