package agentcli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"dont/agent"
	"dont/internal/buildinfo"
	"dont/internal/softwareupdate"
	"dont/shared"
)

var (
	serverURL      = flag.String("server", "ws://localhost:8081/agent", "服务器WebSocket URL")
	agentID        = flag.String("id", "", "代理唯一标识")
	reportInterval = flag.Duration("report", shared.DefaultSystemReportInterval, "主动上报间隔")
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
	showVersion    = flag.Bool("version", false, "显示 Agent 版本")
	showBuild      = flag.Bool("build-info", false, "显示 Agent 发行版构建信息")
)

// Run is shared by both supported Agent build paths so their CLI contracts
// cannot drift apart.
func Run() {
	flag.Parse()
	releaseVersion, releaseBuild := softwareupdate.NormalizeVersion(buildinfo.Current().Version)
	if *showBuild {
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			buildinfo.Info
			AgentVersion string `json:"agentVersion"`
			Kind         string `json:"kind"`
		}{buildinfo.Current(), agent.AgentVersion, "agent"})
		return
	}
	if *showVersion {
		fmt.Println(agent.AgentVersion)
		return
	}
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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if releaseBuild && softwareupdate.AgentPlatform(runtime.GOOS+"-"+runtime.GOARCH) && !softwareupdate.ManagedChild() {
		config, err := filepath.Abs(*configPath)
		if err != nil {
			log.Fatal(err)
		}
		root, err := softwareupdate.DefaultAgentRoot(config)
		if err != nil {
			log.Fatal(err)
		}
		if err := softwareupdate.RunAgentSupervisor(ctx, root, config, releaseVersion, os.Args[1:]); err != nil {
			log.Fatalf("Agent launcher: %v", err)
		}
		return
	}

	if *agentID == "" {
		hostname, err := os.Hostname()
		if err != nil {
			log.Fatalf("无法获取主机名: %v", err)
		}
		*agentID = hostname
	}

	a, err := agent.NewAgent(&agent.Config{
		ServerURL:          *serverURL,
		AgentID:            *agentID,
		ReportInterval:     *reportInterval,
		SecurityKey:        *securityKey,
		KeyFile:            *configPath,
		OperationStateFile: *statePath,
	})
	if err != nil {
		log.Fatalf("创建Agent失败: %v", err)
	}
	if releaseBuild && softwareupdate.ManagedChild() {
		root, err := softwareupdate.DefaultAgentRoot(*configPath)
		if err != nil {
			log.Fatal(err)
		}
		a.ConfigureSoftwareUpdate(softwareupdate.New(softwareupdate.Config{Kind: "agent", Root: root, Current: buildinfo.Current(), BaseVersion: os.Getenv("DST_ADMIN_UPDATE_BASE_VERSION"), Platform: runtime.GOOS + "-" + runtime.GOARCH, Managed: true}))
		closeHealth, err := softwareupdate.ServeAgentHealth(releaseVersion, a.Connected, stop)
		if err != nil {
			log.Fatalf("Agent update health: %v", err)
		}
		defer closeHealth()
	}
	if err := a.Start(); err != nil {
		log.Fatalf("启动Agent失败: %v", err)
	}

	<-ctx.Done()

	log.Println("收到退出信号，正在关闭Agent...")
	a.Stop()
}
