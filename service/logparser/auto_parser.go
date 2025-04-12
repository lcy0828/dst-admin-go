package logparser

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"dont/models"
	"dont/tmux"
)

// AutoParserService 自动日志解析服务
type AutoParserService struct {
	dstSavePath     string
	interval        time.Duration
	stopChan        chan struct{}
	running         bool
	positionManager *PositionManager // 位置管理器
	posLock         sync.Mutex       // 位置映射的锁
}

// NewAutoParserService 创建新的自动日志解析服务
func NewAutoParserService(dstSavePath string, interval time.Duration) *AutoParserService {
	// 使用全局位置管理器
	positionManager := GetGlobalPositionManager(dstSavePath)

	return &AutoParserService{
		dstSavePath:     dstSavePath,
		interval:        interval,
		stopChan:        make(chan struct{}),
		running:         false,
		positionManager: positionManager,
	}
}

// Start 启动自动日志解析服务
func (s *AutoParserService) Start() {
	if s.running {
		log.Printf("[AutoParser] 服务已经在运行中")
		return
	}

	// 全局位置管理器已经启动，不需要再次启动

	s.running = true
	log.Printf("[AutoParser] 自动日志解析服务已启动，检查间隔: %v", s.interval)

	go s.run()
}

// Stop 停止自动日志解析服务
func (s *AutoParserService) Stop() {
	if !s.running {
		return
	}

	// 全局位置管理器由全局管理，不需要在这里停止

	s.running = false
	s.stopChan <- struct{}{}
	log.Printf("[AutoParser] 自动日志解析服务已停止")
}

// run 运行自动日志解析服务
func (s *AutoParserService) run() {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	// 立即执行一次
	s.processAllServerLogs()

	for {
		select {
		case <-s.stopChan:
			return
		case <-ticker.C:
			s.processAllServerLogs()
		}
	}
}

// copyLogFile 复制日志文件
func copyLogFile(src, dst string) error {
	// 打开源文件
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开源文件失败: %v", err)
	}
	defer srcFile.Close()

	// 创建目标文件
	dstFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("创建目标文件失败: %v", err)
	}
	defer dstFile.Close()

	// 复制内容
	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		return fmt.Errorf("复制文件内容失败: %v", err)
	}

	return nil
}

// processAllServerLogs 处理所有服务器的日志
func (s *AutoParserService) processAllServerLogs() {
	// 获取所有运行中的服务器
	// 使用silent=true参数，不输出正常日志
	servers := tmux.GetRunningServers(true)
	log.Printf("[AutoParser] 获取到 %d 个运行中的服务器", len(servers))

	for _, server := range servers {
		// 构建日志文件路径
		logFilePath := s.dstSavePath + "/" + server.ArchiveName + "/" + server.WorldName + "/server_log.txt"
		log.Printf("[AutoParser] 处理服务器日志: %s", logFilePath)

		// 检查文件是否存在
		fileInfo, err := os.Stat(logFilePath)
		if os.IsNotExist(err) {
			log.Printf("[AutoParser] 日志文件不存在: %s", logFilePath)
			continue
		} else if err != nil {
			log.Printf("[AutoParser] 获取文件信息失败: %v", err)
			continue
		}

		// 获取文件大小和修改时间
		currentSize := fileInfo.Size()
		currentModTime := fileInfo.ModTime()

		// 获取上次处理位置
		position := s.positionManager.GetPosition(logFilePath)
		if position == nil {
			// 如果是首次处理该文件，创建新的位置记录
			s.positionManager.UpdatePosition(logFilePath, 0, currentModTime)
			position = s.positionManager.GetPosition(logFilePath)
			log.Printf("[AutoParser] 首次处理文件: %s", logFilePath)
		}

		// 检查文件是否被修改
		if currentSize == position.LastPosition && currentModTime.Equal(position.LastModTime) {
			log.Printf("[AutoParser] 文件未变化，跳过处理: %s", logFilePath)
			continue
		}

		// 检查文件是否被截断（如服务器重启）
		isServerRestart := currentSize < position.LastPosition && !currentModTime.Equal(position.LastModTime)
		if isServerRestart {
			// 服务器重启，备份旧日志文件
			if position.LastPosition > 0 {
				// 创建备份目录
				backupDir := filepath.Join(s.dstSavePath, "logs_backup", server.ArchiveName, server.WorldName)
				if err := os.MkdirAll(backupDir, 0755); err != nil {
					log.Printf("[AutoParser] 创建日志备份目录失败: %v", err)
				} else {
					// 生成备份文件名（使用时间戳）
					// 确保时区信息正确（东八区）
					timestamp := time.Now().Format("20060102_150405")
					backupFileName := fmt.Sprintf("server_log_%s.txt", timestamp)
					backupFilePath := filepath.Join(backupDir, backupFileName)

					// 复制日志文件
					if err := copyLogFile(logFilePath, backupFilePath); err != nil {
						log.Printf("[AutoParser] 备份日志文件失败: %v", err)
					} else {
						log.Printf("[AutoParser] 成功备份日志文件到: %s", backupFilePath)
					}
				}
			}

			log.Printf("[AutoParser] 检测到服务器重启，日志文件被重置。文件大小从 %d 变为 %d。重置读取位置: %s",
				position.LastPosition, currentSize, logFilePath)

			// 添加一条服务器重启的日志
			parser, err := NewLogParser(server.ArchiveName, server.WorldName)
			if err == nil {
				restartMsg := fmt.Sprintf("服务器已重启，日志文件被重置。文件大小从 %d 字节变为 %d 字节",
					position.LastPosition, currentSize)
				parser.SaveLogToDatabase("system", restartMsg, time.Now())
			}

			position.LastPosition = 0
		} else if currentSize < position.LastPosition {
			// 文件被截断，但不是服务器重启
			log.Printf("[AutoParser] 检测到文件被截断，重置读取位置: %s", logFilePath)
			position.LastPosition = 0
		}

		// 打开文件
		file, err := os.Open(logFilePath)
		if err != nil {
			log.Printf("[AutoParser] 打开文件失败: %v", err)
			continue
		}
		defer file.Close()

		// 设置读取位置
		if _, err := file.Seek(position.LastPosition, 0); err != nil {
			log.Printf("[AutoParser] 设置文件读取位置失败: %v", err)
			continue
		}

		// 读取新内容
		newContent := make([]byte, currentSize-position.LastPosition)
		n, err := file.Read(newContent)
		if err != nil {
			log.Printf("[AutoParser] 读取文件内容失败: %v", err)
			continue
		}

		// 如果没有新内容，跳过处理
		if n == 0 {
			log.Printf("[AutoParser] 没有新内容，跳过处理: %s", logFilePath)
			continue
		}

		log.Printf("[AutoParser] 成功读取新内容，大小: %d 字节，总大小: %d 字节", n, currentSize)

		// 创建日志解析器
		parser, err := NewLogParser(server.ArchiveName, server.WorldName)
		if err != nil {
			log.Printf("[AutoParser] 创建日志解析器失败: %v", err)
			continue
		}

		// 处理新内容
		if err := parser.ProcessAndSaveLog(string(newContent[:n])); err != nil {
			log.Printf("[AutoParser] 处理日志内容失败: %v", err)
		} else {
			log.Printf("[AutoParser] 成功处理日志内容")

			// 更新处理位置
			s.positionManager.UpdatePosition(logFilePath, currentSize, currentModTime)
		}
	}

	// 检查数据库中的日志数量
	count, err := models.GetGameLogCount()
	if err != nil {
		log.Printf("[AutoParser] 获取日志数量失败: %v", err)
	} else {
		log.Printf("[AutoParser] 当前数据库中有 %d 条日志记录", count)
	}
}

// 全局自动日志解析服务实例
var globalAutoParserService *AutoParserService

// InitAutoParserService 初始化自动日志解析服务
func InitAutoParserService(dstSavePath string, interval time.Duration) {
	if globalAutoParserService != nil {
		globalAutoParserService.Stop()
	}

	globalAutoParserService = NewAutoParserService(dstSavePath, interval)
	globalAutoParserService.Start()
}

// GetAutoParserService 获取全局自动日志解析服务实例
func GetAutoParserService() *AutoParserService {
	return globalAutoParserService
}
