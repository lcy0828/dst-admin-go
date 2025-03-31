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
		// 强制使用/tmp目录中的文件 - 这是一个几乎所有系统都应该有权限的目录
		*keyFile = "/tmp/agent_server_key.json"
		log.Printf("强制使用/tmp目录中的密钥文件: %s", *keyFile)
		
		// 确保/tmp目录存在并可写
		if err := os.MkdirAll("/tmp", 0755); err != nil {
			log.Printf("警告: 无法确保/tmp目录存在: %v", err)
		}
		
		// 检查文件是否存在，如果存在则输出其状态
		fileInfo, err := os.Stat(*keyFile)
		if err == nil {
			// 文件存在
			isDir := fileInfo.IsDir()
			mode := fileInfo.Mode()
			log.Printf("密钥文件已存在: isDir=%v, 权限=%v, 大小=%d字节", 
				isDir, mode, fileInfo.Size())
		} else if os.IsNotExist(err) {
			// 文件不存在 - 这是预期情况，会在后续创建
			log.Printf("密钥文件不存在，将在初始化时创建")
		} else {
			// 其他错误
			log.Printf("检查密钥文件时出错: %v", err)
		}
		
		// 测试/tmp目录写入权限
		testFile := "/tmp/agent_key_test"
		if err := ioutil.WriteFile(testFile, []byte("test"), 0600); err != nil {
			log.Printf("警告: 无法在/tmp目录写入测试文件: %v", err)
		} else {
			os.Remove(testFile)
			log.Printf("/tmp目录可写，测试成功")
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
