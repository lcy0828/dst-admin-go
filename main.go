package main

import (
	"dont/controller"
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

	"dont/pkg/setting"
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

	// 初始化日志解析器管理器
	logManager := logparser.GetLogParserManager()

	// 启动日志清理任务
	logManager.StartCleanupTask(*logRetentionDays)
	log.Printf("日志清理任务已启动，保留最近 %d 天的日志", *logRetentionDays)

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

		// 添加测试服务器数据
		tmux.AddTestServer()

		// 启动监控服务
		if err := dynamicLogMonitor.Start(); err != nil {
			log.Printf("警告: 启动动态日志监控服务失败: %v", err)
		} else {
			log.Printf("动态日志监控服务已启动，检查间隔: %v", *logCheckInterval)
		}
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

	log.Println("服务器已关闭")
}
