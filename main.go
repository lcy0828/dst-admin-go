package main

import (
	"dont/routers"
	"dont/routers/backup"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"dont/pkg/setting"
)

func main() {
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

	// 等待中断信号以优雅地关闭服务器
	quit := make(chan os.Signal, 1)
	// 接收syscall.SIGINT和syscall.SIGTERM信号
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("正在关闭服务器...")

	log.Println("服务器已关闭")
}
