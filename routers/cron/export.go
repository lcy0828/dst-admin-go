package cron

import (
	"dont/models"
	"dont/pkg/e"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
)

// RegisterCronExportRoutes 注册定时任务导入导出相关路由
func RegisterCronExportRoutes(router *gin.RouterGroup) {
	cronExportGroup := router.Group("/cron/export")
	{
		// 导出任务
		cronExportGroup.POST("", ExportTasks)

		// 导入任务
		cronExportGroup.POST("/import", ImportTasks)

		// 获取导出文件列表
		cronExportGroup.GET("/files", GetExportFiles)

		// 下载导出文件
		cronExportGroup.GET("/download/:filename", DownloadExportFile)

		// 删除导出文件
		cronExportGroup.DELETE("/files/:filename", DeleteExportFile)
	}
}

// ExportTasksRequest 导出任务请求
type ExportTasksRequest struct {
	Description string `json:"description"`
	Filename    string `json:"filename"`
}

// ExportTasks 导出任务
func ExportTasks(c *gin.Context) {
	var req ExportTasksRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的请求参数: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 创建导出目录
	exportDir := "./exports"
	if err := os.MkdirAll(exportDir, 0755); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "创建导出目录失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 生成文件名
	filename := req.Filename
	if filename == "" {
		filename = "cron_tasks_" + time.Now().Format("20060102_150405") + ".json"
	} else if filepath.Ext(filename) != ".json" {
		filename += ".json"
	}

	// 导出任务
	filePath := filepath.Join(exportDir, filename)
	if err := models.ExportTasks(filePath, req.Description); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "导出任务失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "导出任务成功",
		"data": gin.H{
			"filename": filename,
			"filepath": filePath,
		},
	})
}

// ImportTasksRequest 导入任务请求
type ImportTasksRequest struct {
	Filename  string `json:"filename" binding:"required"`
	Overwrite bool   `json:"overwrite"`
}

// ImportTasks 导入任务
func ImportTasks(c *gin.Context) {
	var req ImportTasksRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的请求参数: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 检查文件是否存在
	exportDir := "./exports"
	filePath := filepath.Join(exportDir, req.Filename)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "导入文件不存在",
			"data": nil,
		})
		return
	}

	// 导入任务
	if err := models.ImportTasks(filePath, req.Overwrite); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "导入任务失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "导入任务成功",
		"data": nil,
	})
}

// ExportFileInfo 导出文件信息
type ExportFileInfo struct {
	Filename    string    `json:"filename"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"mod_time"`
	Description string    `json:"description"`
}

// GetExportFiles 获取导出文件列表
func GetExportFiles(c *gin.Context) {
	// 创建导出目录
	exportDir := "./exports"
	if err := os.MkdirAll(exportDir, 0755); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "创建导出目录失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 读取目录
	files, err := os.ReadDir(exportDir)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "读取导出目录失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	// 获取文件信息
	fileInfos := make([]ExportFileInfo, 0, len(files))
	for _, file := range files {
		if filepath.Ext(file.Name()) != ".json" {
			continue
		}

		info, err := file.Info()
		if err != nil {
			continue
		}

		// 读取文件内容，获取描述信息
		filePath := filepath.Join(exportDir, file.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}

		var exportData models.TaskExportData
		if err := json.Unmarshal(data, &exportData); err != nil {
			continue
		}

		fileInfos = append(fileInfos, ExportFileInfo{
			Filename:    file.Name(),
			Size:        info.Size(),
			ModTime:     info.ModTime(),
			Description: exportData.Description,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  e.GetMsg(e.SUCCESS),
		"data": fileInfos,
	})
}

// DownloadExportFile 下载导出文件
func DownloadExportFile(c *gin.Context) {
	filename := c.Param("filename")
	if filename == "" {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的文件名",
			"data": nil,
		})
		return
	}

	// 检查文件是否存在
	exportDir := "./exports"
	filePath := filepath.Join(exportDir, filename)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "文件不存在",
			"data": nil,
		})
		return
	}

	// 下载文件
	c.Header("Content-Description", "File Transfer")
	c.Header("Content-Disposition", "attachment; filename="+filename)
	c.Header("Content-Type", "application/json")
	c.File(filePath)
}

// DeleteExportFile 删除导出文件
func DeleteExportFile(c *gin.Context) {
	filename := c.Param("filename")
	if filename == "" {
		c.JSON(http.StatusOK, gin.H{
			"code": e.INVALID_PARAMS,
			"msg":  "无效的文件名",
			"data": nil,
		})
		return
	}

	// 检查文件是否存在
	exportDir := "./exports"
	filePath := filepath.Join(exportDir, filename)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "文件不存在",
			"data": nil,
		})
		return
	}

	// 删除文件
	if err := os.Remove(filePath); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": e.ERROR,
			"msg":  "删除文件失败: " + err.Error(),
			"data": nil,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": e.SUCCESS,
		"msg":  "删除文件成功",
		"data": nil,
	})
}
