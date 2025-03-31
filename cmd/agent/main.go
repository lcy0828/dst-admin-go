package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dont/agent"
)

func main() {
	// 解析命令行参数
	serverURL := flag.String("server", "", "服务器URL，如: ws://example.com:8080/agent")
	agentID := flag.String("id", "", "代理ID")
	key := flag.String("key", "", "通信安全密钥")
	keyFile := flag.String("keyfile", "./conf/app.conf", "密钥文件路径，默认为./conf/app.conf")
	reportInterval := flag.Duration("report", 10*time.Minute, "数据上报间隔，如: 10m, 1h")
	flag.Parse()

	// 检查必要参数
	if *serverURL == "" {
		log.Fatal("缺少服务器URL，请使用 -server 参数指定")
	}

	if *agentID == "" {
		hostname, err := os.Hostname()
		if err != nil {
			log.Printf("获取主机名失败: %v, 将使用随机ID", err)
			// 生成随机ID
			randBytes := make([]byte, 8)
			rand.Read(randBytes)
			*agentID = fmt.Sprintf("agent-%x", randBytes)
		} else {
			*agentID = fmt.Sprintf("agent-%s", hostname)
		}
		log.Printf("未指定代理ID，将使用生成的ID: %s", *agentID)
	}

	// 创建并启动代理
	config := &agent.Config{
		ServerURL:      *serverURL,
		AgentID:        *agentID,
		ReportInterval: *reportInterval,
		SecurityKey:    *key,
		KeyFile:        *keyFile,
	}

	// 如果未指定通信密钥且未指定密钥文件，则提示用户
	if config.SecurityKey == "" && config.KeyFile == "" {
		log.Println("警告: 未指定通信密钥且未指定密钥文件，将尝试使用默认密钥文件")
	}

	// 创建代理实例
	a, err := agent.NewAgent(config)
	if err != nil {
		log.Fatalf("创建代理失败: %v", err)
	}

	// 连接服务器
	if err := a.Connect(); err != nil {
		log.Fatalf("连接服务器失败: %v", err)
	}

	// 启动代理
	if err := a.Start(); err != nil {
		log.Fatalf("启动代理失败: %v", err)
	}

	// 等待中断信号
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)
	<-signalChan

	// 优雅关闭
	log.Println("接收到中断信号，正在关闭...")
	a.Stop()
	log.Println("已关闭")
} 