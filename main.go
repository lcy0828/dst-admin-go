package main

import (
	"dont/controller"
	"dont/routers"
	"dont/routers/backup"
	"dont/server"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
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
		// 设置密钥文件保存在当前目录
		if *keyFile == "" {
			*keyFile = "./agent_server_key.json"
		}
		log.Printf("使用当前目录中的密钥文件: %s", *keyFile)
		
		// 测试当前目录写入权限
		testFile := "./agent_key_test"
		testErr := ioutil.WriteFile(testFile, []byte("test"), 0600)
		if testErr != nil {
			log.Printf("警告: 无法在当前目录写入测试文件: %v", testErr)
		} else {
			os.Remove(testFile)
			log.Printf("当前目录可写，测试成功")
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
