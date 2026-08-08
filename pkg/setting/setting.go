package setting

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-ini/ini"
)

var (
	Cfg *ini.File

	RunMode string

	HTTPPort     int
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	PageSize    int
	JwtSecret   string
	ConfigPath  string
	ProjectRoot string
)

func init() {
	var err error
	ConfigPath, err = findConfigPath()
	if err != nil {
		log.Fatalf("Fail to locate config: %v", err)
	}
	Cfg, err = ini.Load(ConfigPath)
	if err != nil {
		log.Fatalf("Fail to parse %q: %v", ConfigPath, err)
	}
	ProjectRoot = filepath.Dir(ConfigPath)
	if filepath.Base(ProjectRoot) == "conf" {
		ProjectRoot = filepath.Dir(ProjectRoot)
	}

	LoadBase()
	LoadServer()
	LoadApp()
}

func ResolvePath(value string) string {
	if value == "" || value == ":memory:" || filepath.IsAbs(value) {
		return value
	}
	return filepath.Clean(filepath.Join(ProjectRoot, value))
}

func String(section, key, environment string) string {
	if value := strings.TrimSpace(os.Getenv(environment)); value != "" {
		return value
	}
	return Cfg.Section(section).Key(key).String()
}

func Path(section, key, environment string) string {
	return ResolvePath(String(section, key, environment))
}

func findConfigPath() (string, error) {
	if configured := os.Getenv("DST_ADMIN_CONFIG"); configured != "" {
		absolute, err := filepath.Abs(configured)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(absolute); err != nil {
			return "", err
		}
		return absolute, nil
	}
	current, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(current, "conf", "app.conf")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return "", fmt.Errorf("conf/app.conf was not found from the working directory or its parents; set DST_ADMIN_CONFIG")
}

func LoadBase() {
	RunMode = Cfg.Section("").Key("RUN_MODE").MustString("debug")
}

func LoadServer() {
	sec, err := Cfg.GetSection("server")
	if err != nil {
		log.Fatalf("Fail to get section 'server': %v", err)
	}

	// 优先使用环境变量 PORT
	portEnv := os.Getenv("PORT")
	if portEnv != "" {
		if port, err := strconv.Atoi(portEnv); err == nil {
			HTTPPort = port
			log.Printf("使用环境变量设置端口: %d", HTTPPort)
		} else {
			log.Printf("环境变量 PORT 格式不正确: %s, 使用配置文件端口", portEnv)
			HTTPPort = sec.Key("HTTP_PORT").MustInt(8000)
		}
	} else {
		HTTPPort = sec.Key("HTTP_PORT").MustInt(8000)
	}

	ReadTimeout = time.Duration(sec.Key("READ_TIMEOUT").MustInt(60)) * time.Second
	WriteTimeout = time.Duration(sec.Key("WRITE_TIMEOUT").MustInt(60)) * time.Second
}

func LoadApp() {
	sec, err := Cfg.GetSection("app")
	if err != nil {
		log.Fatalf("Fail to get section 'app': %v", err)
	}

	JwtSecret = sec.Key("JWT_SECRET").String()
	PageSize = sec.Key("PAGE_SIZE").MustInt(10)
}
