package tag

import (
	"github.com/gin-gonic/gin"
	"net/http"
)

// AddTag 添加标签
func AddTag(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "添加标签功能待实现",
	})
}

// DeleteTag 删除标签
func DeleteTag(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "删除标签功能待实现",
	})
}

// EditTag 编辑标签
func EditTag(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "编辑标签功能待实现",
	})
}

// GetTags 获取标签列表
func GetTags(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": 200,
		"msg":    "获取标签列表功能待实现",
		"data":   []string{},
	})
} 