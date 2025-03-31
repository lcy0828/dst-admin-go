package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dont/agent"
)

var (
	serverURL      = flag.String("server", "ws://localhost:8080/agent", "服务器WebSocket URL")
	agentID        = flag.String("id", "", "代理唯一标识")
	reportInterval = flag.Duration("report", 5*time.Minute, "主动上报间隔")
	securityKey    = flag.String("key", "", "通信安全密钥")
)

func main() {
	flag.Parse()

	// 如果未指定AgentID，则使用主机名
	if *agentID == "" {
		hostname, err := os.Hostname()
		if err != nil {
			log.Fatalf("无法获取主机名: %v", err)
		}
		*agentID = hostname
	}

	// 创建Agent配置
	config := &agent.Config{
		ServerURL:      *serverURL,
		AgentID:        *agentID,
		ReportInterval: *reportInterval,
		SecurityKey:    *securityKey,
	}

	// 创建并启动Agent
	a, err := agent.NewAgent(config)
	if err != nil {
		log.Fatalf("创建Agent失败: %v", err)
	}

	if err := a.Start(); err != nil {
		log.Fatalf("启动Agent失败: %v", err)
	}

	log.Printf("Agent已启动，ID: %s, 连接到服务器: %s", *agentID, *serverURL)

	// 设置信号处理，优雅退出
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("收到退出信号，正在关闭Agent...")
	a.Stop()
} 