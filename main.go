package main

import (
	"dont/controller"
	"dont/routers"
	"dont/routers/backup"
	"dont/server"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"dont/pkg/setting"
)

var (
	enableAgentServer = flag.Bool("agent-server", false, "启用Agent-Server通信系统")
	agentServerListen = flag.String("agent-listen", ":8081", "Agent-Server监听地址")
	tlsCert           = flag.String("cert", "", "TLS证书文件路径")
	tlsKey            = flag.String("key", "", "TLS密钥文件路径")
	keyFile           = flag.String("key-file", "", "Agent通信密钥文件路径")
)

func main() {
	flag.Parse()
	
	//models.SaverFiletest()
	
	// 确保备份目录存在
	if err := os.MkdirAll(backup.DstBackupPath, 0755); err != nil {
		log.Printf("警告：无法创建备份目录: %v", err)
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
		// 如果未指定密钥文件，使用默认路径
		if *keyFile == "" {
			// 获取可执行文件所在目录作为基础路径
			execDir, err := filepath.Abs(filepath.Dir(os.Args[0]))
			if err != nil {
				log.Printf("警告：无法获取程序目录: %v，将使用当前目录", err)
				execDir = "."
			}
			
			*keyFile = filepath.Join(execDir, "agent_server_key.json")
			log.Printf("未指定通信密钥文件，使用默认路径: %s", *keyFile)
		} else {
			// 确保使用绝对路径
			absPath, err := filepath.Abs(*keyFile)
			if err != nil {
				log.Printf("警告：无法获取密钥文件的绝对路径: %v，将使用原始路径", err)
			} else {
				*keyFile = absPath
			}
		}
		
		// 检查密钥文件所在目录是否存在，如果不存在则创建
		keyDir := filepath.Dir(*keyFile)
		if err := os.MkdirAll(keyDir, 0755); err != nil {
			log.Printf("警告：无法创建密钥文件目录: %v", err)
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
		log.Printf("使用通信密钥文件: %s", *keyFile)
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

	log.Println("服务器已关闭")
}
