package main

import (
	"dont/controller"
	"dont/cron"
	"dont/models"
	"dont/pkg/commands"
	"dont/pkg/setting"
	"dont/routers"
	"dont/routers/backup"
	"dont/server"
	"dont/service/logmonitor"
	"dont/service/logparser"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var (
	enableAgentServer = flag.Bool("agent-server", false, "启用Agent-Server通信系统")
	agentServerListen = flag.String("agent-listen", ":8081", "Agent-Server监听地址")
	tlsCert           = flag.String("cert", "", "TLS证书文件路径")
	tlsKey            = flag.String("key", "", "TLS密钥文件路径")
	keyFile           = flag.String("key-file", "./conf/app.conf", "Agent通信密钥配置文件路径，默认为conf/app.conf")
	logRetentionDays  = flag.Int("log-retention", 30, "日志保留天数，默认为30天")
	enableDynamicLog  = flag.Bool("dynamic-log", true, "启用动态日志监控，根据服务器状态自动调整日志路径")
	logCheckInterval  = flag.Duration("log-check-interval", 5*time.Second, "日志监控检查间隔，默认为5秒")
)

func main() {
	flag.Parse()

	//models.SaverFiletest()

	// 确保备份目录存在
	if err := os.MkdirAll(backup.DstBackupPath, 0755); err != nil {
		log.Printf("警告：无法创建备份目录: %v", err)
	}

	// 初始化数据库表
	models.InitCronTaskTable()
	models.InitCronTaskLogTable()
	models.InitCommandTable() // 初始化命令表

	// 手动初始化内置命令
	if err := commands.InitBuiltinCommands(); err != nil {
		log.Printf("警告：手动初始化内置命令失败: %v", err)
	} else {
		log.Printf("手动初始化内置命令成功")
	}

	// 初始化日志解析器管理器
	_ = logparser.GetLogParserManager()

	// 初始化定时任务管理器
	taskManager := cron.GetTaskManager()

	// 启动定时任务管理器
	if err := taskManager.Start(); err != nil {
		log.Printf("警告：启动定时任务管理器失败: %v", err)
	} else {
		log.Printf("定时任务管理器已启动")
	}

	// 初始化动态日志监控服务
	var dynamicLogMonitor *logmonitor.DynamicLogMonitor
	if *enableDynamicLog {
		// 从配置文件读取DST存档路径
		dstSavePath := "./Klei/DoNotStarveTogether"
		configFile := "./conf/app.conf"
		if _, err := os.Stat(configFile); !os.IsNotExist(err) {
			if cfg, err := routers.LoadConfig(configFile); err == nil {
				if cfg.Section("paths").HasKey("DST_SAVE_PATH") {
					dstSavePath = cfg.Section("paths").Key("DST_SAVE_PATH").String()
					log.Printf("从配置文件加载DST存档路径: %s", dstSavePath)
				}
			}
		}

		// 创建动态日志监控服务
		dynamicLogMonitor = logmonitor.NewDynamicLogMonitor(
			dstSavePath,
			*logCheckInterval,
			*logRetentionDays,
		)

		// 设置全局实例
		logmonitor.SetDynamicLogMonitor(dynamicLogMonitor)

		// 测试获取全局实例
		testMonitor := logmonitor.GetDynamicLogMonitor()
		if testMonitor == nil {
			log.Printf("警告: 无法获取全局动态日志监控服务实例")
		} else {
			log.Printf("成功获取全局动态日志监控服务实例")
		}

		// 启动监控服务
		if err := dynamicLogMonitor.Start(); err != nil {
			log.Printf("警告: 启动动态日志监控服务失败: %v", err)
		} else {
			log.Printf("动态日志监控服务已启动，检查间隔: %v", *logCheckInterval)
		}

		// 初始化全局位置管理器
		logparser.GetGlobalPositionManager(dstSavePath)
		log.Printf("全局位置管理器已初始化")

		// 初始化自动日志解析服务
		logparser.InitAutoParserService(dstSavePath, 30*time.Second)
		log.Printf("自动日志解析服务已启动，检查间隔: %v", 30*time.Second)
	}

	router := routers.InitRouter()

	s := &http.Server{
		Addr:           fmt.Sprintf(":%d", setting.HTTPPort),
		Handler:        router,
		ReadTimeout:    setting.ReadTimeout,
		WriteTimeout:   setting.WriteTimeout,
		MaxHeaderBytes: 1 << 20,
	}

	// 使用goroutine启动服务器
	go func() {
		if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("服务启动失败: %v", err)
		}
	}()

	// 如果启用了Agent-Server功能，则启动Agent-Server服务器
	var agentServer *server.Server
	if *enableAgentServer {
		// 设置密钥文件路径
		if *keyFile == "" {
			*keyFile = "./conf/app.conf"
		}
		log.Printf("使用配置文件: %s", *keyFile)

		// 确保conf目录存在
		dir := "conf"
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			log.Printf("创建配置目录: %s", dir)
			if err := os.MkdirAll(dir, 0755); err != nil {
				log.Printf("警告: 无法创建配置目录: %v", err)
			}
		}

		// 创建服务器配置
		config := &server.Config{
			ListenAddr: *agentServerListen,
			TLSCert:    *tlsCert,
			TLSKey:     *tlsKey,
			KeyFile:    *keyFile,
		}

		// 创建服务器
		var err error
		agentServer, err = server.NewServer(config)
		if err != nil {
			log.Fatalf("创建Agent服务器失败: %v", err)
		}

		// 设置全局AgentServer实例，供控制器使用
		controller.AgentServer = agentServer

		// 启动服务器
		go func() {
			if err := agentServer.Start(); err != nil {
				log.Fatalf("启动Agent服务器失败: %v", err)
			}
		}()

		log.Printf("Agent服务器已启动，监听地址: %s", *agentServerListen)
		log.Printf("使用配置文件保存通信密钥: %s", *keyFile)
		if *tlsCert != "" && *tlsKey != "" {
			log.Println("Agent服务器TLS已启用")
		}
	}

	// 等待中断信号以优雅地关闭服务器
	quit := make(chan os.Signal, 1)
	// 接收syscall.SIGINT和syscall.SIGTERM信号
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("正在关闭服务器...")

	// 如果Agent-Server服务器正在运行，则停止它
	if agentServer != nil {
		agentServer.Stop()
		controller.AgentServer = nil
		log.Println("Agent服务器已关闭")
	}

	// 如果动态日志监控服务正在运行，则停止它
	if dynamicLogMonitor != nil {
		dynamicLogMonitor.Stop()
		log.Println("动态日志监控服务已关闭")
	}

	// 关闭所有日志解析器，确保缓冲区中的日志被写入数据库
	logparser.ShutdownAllLogParsers()
	log.Println("所有日志解析器已关闭")

	// 关闭全局位置管理器
	logparser.ShutdownGlobalPositionManager()
	log.Println("全局位置管理器已关闭")

	// 关闭 gamelog 包中的位置管理器
	routers.ShutdownGameLogPositionManager()
	log.Println("GameLog 位置管理器已关闭")

	// 关闭统计缓存，确保所有缓存数据被写入数据库
	models.CloseStatCache()
	log.Println("统计缓存已关闭")

	// 停止定时任务管理器
	taskManager.Stop()
	log.Println("定时任务管理器已关闭")

	log.Println("服务器已关闭")
}
