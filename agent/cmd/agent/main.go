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
	serverURL      = flag.String("server", "ws://localhost:8081/agent", "服务器WebSocket URL")
	agentID        = flag.String("id", "", "代理唯一标识")
	reportInterval = flag.Duration("report", 5*time.Minute, "主动上报间隔")
	securityKey    = flag.String("key", "", "通信安全密钥")
	configPath     = flag.String("config", "./conf/app.conf", "Agent 配置文件路径")
	statePath      = flag.String("state", "", "幂等操作状态文件路径")
	attach         = flag.Bool("attach", false, "连接受管 DST 控制台")
	installation   = flag.String("installation", "default", "attach 的 DST 安装 ID")
	cluster        = flag.String("cluster", "", "attach 的 Cluster 目录名")
	shard          = flag.String("shard", "", "attach 的 Shard 目录名")
	writable       = flag.Bool("write", false, "取得限时 maintenance lease 后允许写入")
	attachOwner    = flag.String("owner", "", "可写 attach 操作者")
	attachLease    = flag.Duration("lease", 10*time.Minute, "可写 attach 租约时长（1m-1h）")
)

func main() {
	flag.Parse()
	if *attach {
		socketPath, err := agent.ConsoleAttachSocketPath(*statePath)
		if err != nil {
			log.Fatal(err)
		}
		owner := *attachOwner
		if owner == "" {
			owner = os.Getenv("USER")
		}
		if err := agent.RunConsoleAttach(agent.ConsoleAttachOptions{
			SocketPath: socketPath, InstallationID: *installation, Cluster: *cluster, Shard: *shard,
			Owner: owner, Writable: *writable, LeaseDuration: *attachLease,
		}); err != nil {
			log.Fatal(err)
		}
		return
	}

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
		ServerURL:          *serverURL,
		AgentID:            *agentID,
		ReportInterval:     *reportInterval,
		SecurityKey:        *securityKey,
		KeyFile:            *configPath,
		OperationStateFile: *statePath,
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
