package backup

import (
	"archive/zip"
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// 存档路径和备份路径
	DstSavePath    = "/root/DST/Klei/DoNotStarveTogether" // DST存档目录
	DstBackupPath  = "/root/DST/Klei/dst-archive"         // DST备份目录
)

// ArchiveBackupResponse 备份响应结构
type ArchiveBackupResponse struct {
	Status int         `json:"status"`
	Msg    string      `json:"msg"`
	Data   interface{} `json:"data,omitempty"`
}

// BackupInfo 备份信息结构
type BackupInfo struct {
	Name         string `json:"name"`         // 备份文件名
	ArchiveName  string `json:"archive_name"` // 存档名称
	Size         int64  `json:"size"`         // 文件大小(字节)
	SizeFormatted string `json:"size_formatted"` // 格式化的大小
	CreateTime   string `json:"create_time"`  // 创建时间
}

// CreateBackup 创建存档备份
func CreateBackup() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 定义请求参数结构体
		type CreateBackupRequest struct {
			ArchiveName string `json:"archive" binding:"required"`
		}
		
		var req CreateBackupRequest
		
		// 从请求体中获取参数
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusOK, ArchiveBackupResponse{
				Status: 400,
				Msg:    "无效的请求参数: " + err.Error(),
			})
			return
		}
		
		// 获取存档名称
		archiveName := req.ArchiveName
		if archiveName == "" {
			c.JSON(http.StatusOK, ArchiveBackupResponse{
				Status: 400,
				Msg:    "存档名称不能为空",
			})
			return
		}

		// 检查存档是否存在
		archivePath := filepath.Join(DstSavePath, archiveName)
		if _, err := os.Stat(archivePath); os.IsNotExist(err) {
			c.JSON(http.StatusOK, ArchiveBackupResponse{
				Status: 404,
				Msg:    "指定的存档不存在",
			})
			return
		}

		// 创建备份目录(如果不存在)
		backupDir := filepath.Join(DstBackupPath, archiveName)
		if err := os.MkdirAll(backupDir, 0755); err != nil {
			c.JSON(http.StatusOK, ArchiveBackupResponse{
				Status: 500,
				Msg:    "创建备份目录失败: " + err.Error(),
			})
			return
		}

		// 生成备份文件名(存档名-年月日时分.zip)
		timeStr := time.Now().Format("200601021504")
		backupFileName := fmt.Sprintf("%s-%s.zip", archiveName, timeStr)
		backupFilePath := filepath.Join(backupDir, backupFileName)

		// 创建zip文件
		zipFile, err := os.Create(backupFilePath)
		if err != nil {
			c.JSON(http.StatusOK, ArchiveBackupResponse{
				Status: 500,
				Msg:    "创建备份文件失败: " + err.Error(),
			})
			return
		}
		defer zipFile.Close()

		// 创建zip writer
		zipWriter := zip.NewWriter(zipFile)
		defer zipWriter.Close()

		// 遍历存档目录，添加文件到zip
		err = filepath.Walk(archivePath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			// 获取相对路径
			relPath, err := filepath.Rel(archivePath, path)
			if err != nil {
				return err
			}

			// 跳过根目录
			if relPath == "." {
				return nil
			}

			// 创建header
			header, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}

			// 设置相对路径
			header.Name = relPath

			// 设置压缩方法
			if !info.IsDir() {
				header.Method = zip.Deflate
			}

			// 创建writer
			writer, err := zipWriter.CreateHeader(header)
			if err != nil {
				return err
			}

			// 如果是目录，直接返回
			if info.IsDir() {
				return nil
			}

			// 打开源文件
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()

			// 复制内容到zip文件
			_, err = io.Copy(writer, file)
			return err
		})

		if err != nil {
			c.JSON(http.StatusOK, ArchiveBackupResponse{
				Status: 500,
				Msg:    "创建备份过程中出错: " + err.Error(),
			})
			return
		}

		// 获取备份文件信息
		fileInfo, err := os.Stat(backupFilePath)
		if err != nil {
			c.JSON(http.StatusOK, ArchiveBackupResponse{
				Status: 200,
				Msg:    "备份创建成功，但无法获取文件信息",
				Data: map[string]string{
					"backup_file": backupFileName,
				},
			})
			return
		}

		// 返回成功信息
		c.JSON(http.StatusOK, ArchiveBackupResponse{
			Status: 200,
			Msg:    "备份创建成功",
			Data: map[string]interface{}{
				"backup_file": backupFileName,
				"size":        fileInfo.Size(),
				"size_formatted": formatFileSize(fileInfo.Size()),
				"create_time": fileInfo.ModTime().Format("2006-01-02 15:04:05"),
			},
		})
	}
}

// ListBackups 获取存档备份列表
func ListBackups() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 获取存档名称(可选参数)
		archiveName := c.Query("archive")
		
		// 检查备份根目录是否存在
		if _, err := os.Stat(DstBackupPath); os.IsNotExist(err) {
			// 目录不存在，返回空列表
			c.JSON(http.StatusOK, ArchiveBackupResponse{
				Status: 200,
				Msg:    "备份目录不存在",
				Data:   map[string][]BackupInfo{},
			})
			return
		}
		
		// 如果指定了存档名称，仅获取该存档的备份
		if archiveName != "" {
			backupDir := filepath.Join(DstBackupPath, archiveName)
			if _, err := os.Stat(backupDir); os.IsNotExist(err) {
				// 目录不存在，返回空列表
				c.JSON(http.StatusOK, ArchiveBackupResponse{
					Status: 200,
					Msg:    "指定存档的备份目录不存在",
					Data:   map[string][]BackupInfo{},
				})
				return
			}
			
			// 获取指定存档的备份
			backups, err := getArchiveBackups(archiveName)
			if err != nil {
				c.JSON(http.StatusOK, ArchiveBackupResponse{
					Status: 500,
					Msg:    "读取备份目录失败: " + err.Error(),
					Data:   map[string][]BackupInfo{},
				})
				return
			}
			
			// 返回结果，按存档名称归组
			c.JSON(http.StatusOK, ArchiveBackupResponse{
				Status: 200,
				Msg:    "获取备份列表成功",
				Data:   map[string][]BackupInfo{archiveName: backups},
			})
			return
		}
		
		// 如果未指定存档名称，获取所有存档的备份
		archivesMap := make(map[string][]BackupInfo)
		
		// 读取备份根目录下的所有存档目录
		archives, err := ioutil.ReadDir(DstBackupPath)
		if err != nil {
			c.JSON(http.StatusOK, ArchiveBackupResponse{
				Status: 500,
				Msg:    "读取备份目录失败: " + err.Error(),
				Data:   map[string][]BackupInfo{},
			})
			return
		}
		
		// 遍历每个存档目录
		for _, archive := range archives {
			if archive.IsDir() {
				// 获取该存档的所有备份
				backups, err := getArchiveBackups(archive.Name())
				if err != nil {
					// 如果读取某个存档的备份出错，记录错误但继续处理其他存档
					log.Printf("读取存档 %s 的备份失败: %v", archive.Name(), err)
					continue
				}
				
				// 只有当有备份时才添加到结果中
				if len(backups) > 0 {
					archivesMap[archive.Name()] = backups
				}
			}
		}
		
		// 返回所有存档的备份，按存档名称归组
		c.JSON(http.StatusOK, ArchiveBackupResponse{
			Status: 200,
			Msg:    "获取备份列表成功",
			Data:   archivesMap,
		})
	}
}

// getArchiveBackups 获取指定存档的所有备份
func getArchiveBackups(archiveName string) ([]BackupInfo, error) {
	backupDir := filepath.Join(DstBackupPath, archiveName)
	
	// 读取备份目录
	files, err := ioutil.ReadDir(backupDir)
	if err != nil {
		return nil, err
	}
	
	// 提取zip文件信息
	var backups []BackupInfo
	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".zip") {
			backups = append(backups, BackupInfo{
				Name:          file.Name(),
				ArchiveName:   archiveName,
				Size:          file.Size(),
				SizeFormatted: formatFileSize(file.Size()),
				CreateTime:    file.ModTime().Format("2006-01-02 15:04:05"),
			})
		}
	}
	
	// 按创建时间倒序排序
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].CreateTime > backups[j].CreateTime
	})
	
	return backups, nil
}

// DownloadBackup 下载备份文件
func DownloadBackup() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 获取存档名称和备份文件名
		archiveName := c.Query("archive")
		backupName := c.Query("backup")

		if archiveName == "" || backupName == "" {
			c.JSON(http.StatusOK, ArchiveBackupResponse{
				Status: 400,
				Msg:    "缺少存档名称或备份文件名参数",
			})
			return
		}

		// 验证备份文件名
		if !strings.HasSuffix(backupName, ".zip") {
			c.JSON(http.StatusOK, ArchiveBackupResponse{
				Status: 400,
				Msg:    "备份文件必须是.zip格式",
			})
			return
		}
		
		// 防止路径遍历攻击
		if strings.Contains(backupName, "..") || strings.Contains(archiveName, "..") {
			c.JSON(http.StatusOK, ArchiveBackupResponse{
				Status: 403,
				Msg:    "无效的文件路径",
			})
			return
		}

		// 构建备份文件路径
		backupFilePath := filepath.Join(DstBackupPath, archiveName, backupName)

		// 检查文件是否存在
		if _, err := os.Stat(backupFilePath); os.IsNotExist(err) {
			c.JSON(http.StatusOK, ArchiveBackupResponse{
				Status: 404,
				Msg:    "备份文件不存在",
			})
			return
		}

		// 设置响应头
		c.Header("Content-Description", "File Transfer")
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", backupName))
		c.Header("Content-Type", "application/zip")
		c.Header("Content-Transfer-Encoding", "binary")
		c.Header("Expires", "0")
		c.Header("Cache-Control", "must-revalidate")
		c.Header("Pragma", "public")

		// 发送文件
		c.File(backupFilePath)
	}
}

// formatFileSize 格式化文件大小
func formatFileSize(size int64) string {
	const (
		B  = 1
		KB = 1024 * B
		MB = 1024 * KB
		GB = 1024 * MB
	)

	if size < KB {
		return fmt.Sprintf("%d B", size)
	} else if size < MB {
		return fmt.Sprintf("%.2f KB", float64(size)/float64(KB))
	} else if size < GB {
		return fmt.Sprintf("%.2f MB", float64(size)/float64(MB))
	} else {
		return fmt.Sprintf("%.2f GB", float64(size)/float64(GB))
	}
} 