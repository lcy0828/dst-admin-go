package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"dont/server"
)

var (
	listenAddr = flag.String("listen", ":8080", "监听地址")
	tlsCert    = flag.String("cert", "", "TLS证书文件路径")
	tlsKey     = flag.String("key", "", "TLS密钥文件路径")
)

func main() {
	flag.Parse()

	// 创建服务器配置
	config := &server.Config{
		ListenAddr: *listenAddr,
		TLSCert:    *tlsCert,
		TLSKey:     *tlsKey,
	}

	// 创建服务器
	s, err := server.NewServer(config)
	if err != nil {
		log.Fatalf("创建服务器实例失败: %v", err)
	}

	// 启动服务器（在goroutine中启动，这样不会阻塞）
	go func() {
		if err := s.Start(); err != nil {
			log.Fatalf("启动服务器失败: %v", err)
		}
	}()

	log.Printf("服务器已启动，监听地址: %s", *listenAddr)
	if *tlsCert != "" && *tlsKey != "" {
		log.Println("TLS已启用")
	}

	// 启动命令行界面
	go startCLI(s)

	// 设置信号处理，优雅退出
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("收到退出信号，正在关闭服务器...")
	s.Stop()
}

// 启动简单的命令行界面
func startCLI(s *server.Server) {
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Println("服务器命令行界面已启动，输入'help'查看可用命令")

	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}

		input := scanner.Text()
		parts := strings.Fields(input)
		if len(parts) == 0 {
			continue
		}

		command := parts[0]

		switch command {
		case "help":
			fmt.Println("可用命令:")
			fmt.Println("  list              - 列出所有已连接的Agent")
			fmt.Println("  info <agent_id>   - 显示指定Agent的详细信息")
			fmt.Println("  report <agent_id> <type>   - 请求Agent进行被动上报")
			fmt.Println("  exit              - 退出服务器")

		case "list":
			agentInfo := s.GetAllAgentInfo()
			if len(agentInfo) == 0 {
				fmt.Println("当前没有Agent连接")
			} else {
				fmt.Println("已连接的Agent:")
				for id, info := range agentInfo {
					hostname, ok := info["hostname"].(string)
					if !ok {
						hostname = "未知"
					}
					os, ok := info["os"].(string)
					if !ok {
						os = "未知"
					}
					fmt.Printf("  %s (%s, %s)\n", id, hostname, os)
				}
			}

		case "info":
			if len(parts) < 2 {
				fmt.Println("用法: info <agent_id>")
				continue
			}
			agentID := parts[1]
			agentInfo := s.GetAllAgentInfo()
			info, exists := agentInfo[agentID]
			if !exists {
				fmt.Printf("Agent '%s' 不存在或未连接\n", agentID)
				continue
			}

			fmt.Printf("Agent '%s' 详细信息:\n", agentID)
			for k, v := range info {
				fmt.Printf("  %s: %v\n", k, v)
			}

		case "report":
			if len(parts) < 3 {
				fmt.Println("用法: report <agent_id> <report_type>")
				continue
			}
			agentID := parts[1]
			reportType := parts[2]

			params := make(map[string]interface{})
			err := s.RequestPassiveReport(agentID, reportType, params)
			if err != nil {
				fmt.Printf("请求上报失败: %v\n", err)
				continue
			}
			fmt.Printf("已请求 '%s' 类型的上报\n", reportType)

		case "exit":
			fmt.Println("退出服务器...")
			os.Exit(0)
			return

		default:
			fmt.Printf("未知命令: %s\n", command)
			fmt.Println("输入'help'查看可用命令")
		}
	}
}
