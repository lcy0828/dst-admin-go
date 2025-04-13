package dstserver

import (
	"dont/tmux"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-ini/ini"
)

// 获取steamcmd路径
func getSteamCmdPath() string {
	// 首先从 mod 部分获取
	configFile := "./conf/app.conf"
	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		if cfg, err := ini.Load(configFile); err == nil {
			modSection := cfg.Section("mod")
			if modSection.HasKey("STEAM_CMD_PATH") {
				path := modSection.Key("STEAM_CMD_PATH").String()
				log.Printf("[DST-SERVER] 从配置文件加载steamcmd路径: %s", path)
				return path
			}
		}
	}

	// 默认路径
	return "./steamcmd"
}

// 获取游戏服务器路径
func getDstServerPath() string {
	// 从 paths 部分获取
	configFile := "./conf/app.conf"
	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		if cfg, err := ini.Load(configFile); err == nil {
			pathsSection := cfg.Section("paths")
			if pathsSection.HasKey("DST_SERVER_PATH") {
				path := pathsSection.Key("DST_SERVER_PATH").String()
				// 在macOS上需要截取路径
				if strings.Contains(path, ".app/Contents/MacOS") {
					path = strings.Split(path, ".app/Contents/MacOS")[0]
					log.Printf("[DST-SERVER] 在macOS上截取游戏服务器路径: %s", path)
				} else {
					log.Printf("[DST-SERVER] 从配置文件加载游戏服务器路径: %s", path)
				}
				return path
			}
		}
	}

	// 默认路径
	return "./dstserver"
}

// UpdateServerRequest 更新服务器请求结构
type UpdateServerRequest struct {
	Force bool `json:"force"` // 是否强制更新
}

// UpdateServer 更新游戏服务器
func UpdateServer(c *gin.Context) {
	clientIP := c.ClientIP()
	log.Printf("[API][UpdateServer] 收到更新游戏服务器请求 IP: %s", clientIP)

	var req UpdateServerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Printf("[API][UpdateServer] 解析请求参数失败: %v IP: %s", err, clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "请求参数错误: " + err.Error(),
		})
		return
	}

	// 获取路径
	steamCmdPath := getSteamCmdPath()
	dstServerPath := getDstServerPath()

	// 检查steamcmd路径是否存在
	if _, err := os.Stat(filepath.Join(steamCmdPath, "steamcmd.sh")); os.IsNotExist(err) {
		log.Printf("[API][UpdateServer] steamcmd路径不存在: %s IP: %s", steamCmdPath, clientIP)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("steamcmd路径不存在: %s", steamCmdPath),
		})
		return
	}

	// 检查是否有服务器正在运行
	runningServers, err := tmux.ListDSTServers()
	if err != nil {
		log.Printf("[API][UpdateServer] 获取运行中的服务器列表时发生错误: %v IP: %s", err, clientIP)

		// 检查错误消息是否包含"failed to list sessions"
		if strings.Contains(err.Error(), "failed to list sessions") {
			log.Printf("[API][UpdateServer] 没有运行中的tmux会话，返回空列表 IP: %s", clientIP)

			// 返回空列表
			runningServers = []tmux.ServerInfo{}
		} else {
			// 其他错误仍然返回500
			c.JSON(http.StatusInternalServerError, gin.H{
				"status": 500,
				"msg":    "获取运行中的服务器列表失败: " + err.Error(),
			})
			return
		}
	}

	// 如果有服务器正在运行且不是强制更新，则返回错误
	if len(runningServers) > 0 && !req.Force {
		log.Printf("[API][UpdateServer] 有服务器正在运行，无法更新 IP: %s", clientIP)
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "有服务器正在运行，请先停止所有服务器或使用强制更新",
			"data": gin.H{
				"running_servers": runningServers,
			},
		})
		return
	}

	// 如果是强制更新且有服务器正在运行，则先停止所有服务器
	if len(runningServers) > 0 && req.Force {
		log.Printf("[API][UpdateServer] 强制更新，正在停止所有运行中的服务器 IP: %s", clientIP)

		// 创建一个通道用于等待所有服务器停止
		stopChan := make(chan bool, len(runningServers))

		// 停止所有服务器
		for _, server := range runningServers {
			go func(sessionName string) {
				// 获取服务器实例
				parts := strings.Split(sessionName, "_")
				if len(parts) < 3 || parts[0] != "dstserver" {
					log.Printf("[API][UpdateServer] 会话名称格式不正确: %s", sessionName)
					stopChan <- false
					return
				}

				// 创建服务器实例
				dstServer, err := tmux.NewDSTServer(
					parts[1],
					parts[2],
					"", // 这些参数在停止服务器时不重要
					"",
					"",
					"",
				)

				if err != nil {
					log.Printf("[API][UpdateServer] 创建服务器实例失败: %v 会话名: %s", err, sessionName)
					stopChan <- false
					return
				}

				// 尝试优雅停止
				err = dstServer.Stop()
				if err != nil {
					log.Printf("[API][UpdateServer] 优雅停止服务器失败: %v 会话名: %s", err, sessionName)

					// 尝试强制停止
					err = dstServer.KillSession()
					if err != nil {
						log.Printf("[API][UpdateServer] 强制停止服务器失败: %v 会话名: %s", err, sessionName)
						stopChan <- false
						return
					}
				}

				stopChan <- true
			}(server.SessionName)
		}

		// 等待所有服务器停止
		stoppedCount := 0
		for i := 0; i < len(runningServers); i++ {
			if <-stopChan {
				stoppedCount++
			}
		}

		log.Printf("[API][UpdateServer] 已停止 %d/%d 个服务器 IP: %s", stoppedCount, len(runningServers), clientIP)

		// 等待一段时间确保所有服务器完全停止
		time.Sleep(5 * time.Second)
	}

	// 创建一个唯一的会话名称
	sessionName := fmt.Sprintf("dst_update_%d", time.Now().Unix())

	// 构建更新命令
	log.Printf("[API][UpdateServer] 使用路径 - steamcmd: %s, 游戏服务器: %s", steamCmdPath, dstServerPath)
	updateCmd := fmt.Sprintf("cd %s && ./steamcmd.sh +force_install_dir %s +login anonymous +app_update 343050 validate +quit",
		steamCmdPath, dstServerPath)

	log.Printf("[API][UpdateServer] 开始更新游戏服务器 命令: %s IP: %s", updateCmd, clientIP)

	// 使用exec.Command执行命令
	cmd := exec.Command("bash", "-c", updateCmd)

	// 创建管道获取命令输出
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("[API][UpdateServer] 创建标准输出管道失败: %v IP: %s", err, clientIP)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "创建标准输出管道失败: " + err.Error(),
		})
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("[API][UpdateServer] 创建标准错误管道失败: %v IP: %s", err, clientIP)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "创建标准错误管道失败: " + err.Error(),
		})
		return
	}

	// 启动命令
	if err := cmd.Start(); err != nil {
		log.Printf("[API][UpdateServer] 启动更新命令失败: %v IP: %s", err, clientIP)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    "启动更新命令失败: " + err.Error(),
		})
		return
	}

	// 初始化状态
	status := UpdateStatus{
		StartTime:   time.Now(),
		IsRunning:   true,
		IsCompleted: false,
		Progress:    "0%",
		LastOutput:  "正在启动更新进程...",
	}

	// 记录状态
	updateStatus(sessionName, status)

	// 立即返回响应，让更新在后台进行
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "游戏服务器更新已开始，请稍后查看更新状态",
		"data": gin.H{
			"session_name": sessionName,
		},
	})

	// 在后台处理命令输出和完成
	go func() {
		// 读取并记录输出
		go func() {
			buf := make([]byte, 1024)
			for {
				n, err := stdout.Read(buf)
				if n > 0 {
					output := string(buf[:n])
					log.Printf("[DST-UPDATE][STDOUT] %s", output)

					// 更新状态
					status, exists := getStatus(sessionName)
					if exists {
						// 提取进度信息
						if strings.Contains(output, "progress:") {
							parts := strings.Split(output, "progress:")
							if len(parts) > 1 {
								progressPart := strings.TrimSpace(parts[1])
								progressEnd := strings.Index(progressPart, " ")
								if progressEnd > 0 {
									progressValue := progressPart[:progressEnd]
									status.Progress = progressValue + "%"
								}
							}
						}

						// 更新最后输出
						status.LastOutput = strings.TrimSpace(output)
						updateStatus(sessionName, status)
					}
				}
				if err != nil {
					break
				}
			}
		}()

		go func() {
			buf := make([]byte, 1024)
			for {
				n, err := stderr.Read(buf)
				if n > 0 {
					output := string(buf[:n])
					log.Printf("[DST-UPDATE][STDERR] %s", output)

					// 更新状态中的错误信息
					status, exists := getStatus(sessionName)
					if exists {
						status.Error = output
						updateStatus(sessionName, status)
					}
				}
				if err != nil {
					break
				}
			}
		}()

		// 等待命令完成
		err := cmd.Wait()

		// 更新最终状态
		status, exists := getStatus(sessionName)
		if exists {
			status.IsRunning = false
			status.EndTime = time.Now()

			if err != nil {
				log.Printf("[API][UpdateServer] 更新命令执行失败: %v", err)
				status.IsCompleted = false
				status.Error = err.Error()
			} else {
				log.Printf("[API][UpdateServer] 游戏服务器更新完成")
				status.IsCompleted = true
				status.Progress = "100%"
			}

			updateStatus(sessionName, status)

			// 设置一个定时器，在一定时间后清除状态记录，避免内存泄漏
			go func() {
				time.Sleep(30 * time.Minute) // 30分钟后清除
				updateStatusMutex.Lock()
				delete(updateStatusMap, sessionName)
				updateStatusMutex.Unlock()
				log.Printf("[API][UpdateServer] 清除更新状态记录: %s", sessionName)
			}()
		}
	}()
}

// 全局变量记录更新状态
var (
	updateStatusMap = make(map[string]UpdateStatus)
	updateStatusMutex sync.Mutex
)

// UpdateStatus 更新状态
type UpdateStatus struct {
	StartTime   time.Time // 开始时间
	EndTime     time.Time // 结束时间
	IsRunning   bool      // 是否正在运行
	IsCompleted bool      // 是否已完成
	Progress    string    // 进度信息
	LastOutput  string    // 最后输出
	Error       string    // 错误信息（如果有）
}

// 更新状态
func updateStatus(sessionName string, status UpdateStatus) {
	updateStatusMutex.Lock()
	defer updateStatusMutex.Unlock()
	updateStatusMap[sessionName] = status
}

// 获取状态
func getStatus(sessionName string) (UpdateStatus, bool) {
	updateStatusMutex.Lock()
	defer updateStatusMutex.Unlock()
	status, exists := updateStatusMap[sessionName]
	return status, exists
}

// GetUpdateStatus 获取更新状态
func GetUpdateStatus(c *gin.Context) {
	clientIP := c.ClientIP()
	sessionName := c.Query("session_name")

	log.Printf("[API][GetUpdateStatus] 收到获取更新状态请求 会话名: %s IP: %s", sessionName, clientIP)

	if sessionName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"status": 400,
			"msg":    "缺少session_name参数",
		})
		return
	}

	// 从状态映射中获取状态
	status, exists := getStatus(sessionName)

	if !exists {
		// 没有找到状态记录，可能是无效的会话名或者更新已经完成很久
		c.JSON(http.StatusOK, gin.H{
			"status": 200,
			"msg":    "未找到更新任务或更新已完成",
			"data": gin.H{
				"is_running": false,
				"is_completed": true,
			},
		})
		return
	}

	// 生成状态消息
	var statusMsg string
	if status.IsRunning {
		statusMsg = "更新正在进行中"
	} else if status.IsCompleted {
		statusMsg = "更新已完成"
	} else {
		statusMsg = "更新失败"
	}

	// 返回状态信息
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    statusMsg,
		"data": gin.H{
			"is_running":   status.IsRunning,
			"is_completed": status.IsCompleted,
			"start_time":   status.StartTime.Format("2006-01-02 15:04:05"),
			"progress":     status.Progress,
			"last_output":  status.LastOutput,
			"error":        status.Error,
		},
	})
}
