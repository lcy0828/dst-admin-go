package main

import (
	"dont/tmux"
	"fmt"
	"log"
)

func main2() {
	// 创建一个新的饥荒服务器实例
	server, err := tmux.NewDSTServer(
		"TestArchive",
		"TestWorld",
		"./dstserver/ugc_mods",
		"./Klei",
		"DoNotStarveTogether",
		"64",
	)
	if err != nil {
		log.Fatalf("创建服务器实例失败: %v", err)
	}

	// 检查服务器是否在运行
	running, err := server.IsRunning()
	if err != nil {
		log.Fatalf("检查服务器状态失败: %v", err)
	}
	fmt.Printf("服务器运行状态: %v\n", running)

	// 如果服务器未运行，则启动它
	if !running {
		fmt.Println("正在启动服务器...")
		if err := server.Start(); err != nil {
			log.Fatalf("启动服务器失败: %v", err)
		}
		fmt.Println("服务器已启动")
	}

	// 列出所有饥荒服务器会话
	sessions, err := tmux.ListDSTServers()
	if err != nil {
		log.Fatalf("获取服务器列表失败: %v", err)
	}
	fmt.Println("当前运行的服务器会话:")
	for _, session := range sessions {
		fmt.Printf("- %s\n", session)
	}

	// 向服务器发送命令
	fmt.Println("向服务器发送命令...")
	if err := server.SendCommand("c_announce('Hello from Go!')"); err != nil {
		log.Fatalf("发送命令失败: %v", err)
	}
	fmt.Println("命令已发送")

	// 停止服务器
	fmt.Println("正在停止服务器...")
	if err := server.Stop(); err != nil {
		log.Fatalf("停止服务器失败: %v", err)
	}
	fmt.Println("已发送停止命令")
}
