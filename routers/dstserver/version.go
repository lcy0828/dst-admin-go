package dstserver

import (
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gocolly/colly"
)

// VersionInfo 版本信息结构
type VersionInfo struct {
	Version     string `json:"version"`      // 版本号
	BuildNumber string `json:"build_number"` // 构建号
	ReleaseDate string `json:"release_date"` // 发布日期
	IsTest      bool   `json:"is_test"`      // 是否为测试版
	UpdateType  string `json:"update_type"`  // 更新类型
	UpdateURL   string `json:"update_url"`   // 更新页面URL
}

// GetDSTVersion 获取饥荒联机版最新版本信息
func GetDSTVersion(c *gin.Context) {
	// 获取查询参数
	skipTest := c.DefaultQuery("skip_test", "false") == "true"

	log.Printf("[API][GetDSTVersion] 收到获取饥荒版本请求，跳过测试版: %v", skipTest)

	// 创建一个新的收集器
	collector := colly.NewCollector(
		colly.AllowedDomains("forums.kleientertainment.com"),
	)

	// 设置超时
	collector.SetRequestTimeout(10 * time.Second)

	// 添加请求头
	collector.OnRequest(func(r *colly.Request) {
		r.Headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
		log.Printf("[API][GetDSTVersion] 正在访问: %s", r.URL.String())
	})

	// 用于存储所有找到的版本信息
	var allVersions []VersionInfo
	var pinnedVersions []VersionInfo
	var regularVersions []VersionInfo

	// 在找到带有版本信息的元素时调用
	collector.OnHTML("li.cCmsRecord_row", func(e *colly.HTMLElement) {
		// 获取版本号
		versionNumber := e.ChildText("h3.ipsType_sectionHead")
		if versionNumber == "" {
			return
		}

		// 清理版本号（移除可能的Test标记和空白）
		versionNumber = strings.TrimSpace(versionNumber)
		versionNumber = strings.Split(versionNumber, "\n")[0]
		versionNumber = strings.TrimSpace(versionNumber)

		// 检查是否为测试版
		isTest := e.ChildText("span.ipsBadge") == "Test"

		// 如果需要跳过测试版且当前是测试版，则跳过
		if skipTest && isTest {
			return
		}

		// 获取版本链接和构建号
		versionURL := e.ChildAttr("a.cRelease", "href")

		// 提取构建号 (r数字)
		buildNumber := ""
		buildRegex := regexp.MustCompile(`r(\d+)`)
		buildMatches := buildRegex.FindStringSubmatch(versionURL)
		if len(buildMatches) > 1 {
			buildNumber = buildMatches[1]
		}

		// 获取发布日期
		releaseDate := e.ChildText("div.ipsDataItem_meta")
		releaseDate = strings.TrimSpace(releaseDate)
		releaseDate = strings.TrimPrefix(releaseDate, "Released ")
		releaseDate = strings.TrimSuffix(releaseDate, "...")

		// 确定更新类型
		updateType := "Regular"
		if e.DOM.Find("span.cUpdate_hotfix").Length() > 0 {
			updateType = "Hotfix"
		}

		// 检查是否为置顶版本
		isPinned := e.DOM.Find("span.ipsBadge_icon i.fa-thumb-tack").Length() > 0

		// 创建版本信息
		versionInfo := VersionInfo{
			Version:     versionNumber,
			BuildNumber: buildNumber,
			ReleaseDate: releaseDate,
			IsTest:      isTest,
			UpdateType:  updateType,
			UpdateURL:   versionURL,
		}

		log.Printf("[API][GetDSTVersion] 找到版本信息: %s (r%s), 类型: %s, 测试版: %v, 置顶: %v",
			versionInfo.Version, versionInfo.BuildNumber, versionInfo.UpdateType, versionInfo.IsTest, isPinned)

		// 将版本信息添加到相应的列表中
		allVersions = append(allVersions, versionInfo)

		if isPinned {
			pinnedVersions = append(pinnedVersions, versionInfo)
		} else {
			regularVersions = append(regularVersions, versionInfo)
		}
	})

	// 处理错误
	collector.OnError(func(r *colly.Response, err error) {
		log.Printf("[API][GetDSTVersion] 访问 %s 时出错: %s", r.Request.URL, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("获取版本信息失败: %v", err),
		})
	})

	// 访问饥荒更新页面
	err := collector.Visit("https://forums.kleientertainment.com/game-updates/dst/")
	if err != nil {
		log.Printf("[API][GetDSTVersion] 访问更新页面失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": 500,
			"msg":    fmt.Sprintf("访问更新页面失败: %v", err),
		})
		return
	}

	// 如果没有找到任何版本信息
	if len(allVersions) == 0 {
		c.JSON(http.StatusNotFound, gin.H{
			"status": 404,
			"msg":    "未找到版本信息",
		})
		return
	}

	// 找到最新的版本
	var latestVersion VersionInfo
	var latestReleaseVersion VersionInfo

	// 处理置顶版本
	if len(pinnedVersions) > 0 {
		// 默认使用第一个置顶版本
		latestPinnedVersion := pinnedVersions[0]

		// 如果有多个置顶版本，找出版本号最大的
		for _, v := range pinnedVersions {
			pinnedVersionNum, _ := strconv.Atoi(v.Version)
			latestPinnedVersionNum, _ := strconv.Atoi(latestPinnedVersion.Version)

			if pinnedVersionNum > latestPinnedVersionNum {
				latestPinnedVersion = v
			}
		}

		// 如果置顶版本不是测试版，则将其设置为最新正式版
		if !latestPinnedVersion.IsTest {
			latestReleaseVersion = latestPinnedVersion
		}

		// 如果不需要跳过测试版，或者置顶版本不是测试版，则将其设置为最新版本
		if !skipTest || !latestPinnedVersion.IsTest {
			latestVersion = latestPinnedVersion
		}
	}

	// 处理非置顶版本
	if len(regularVersions) > 0 {
		// 默认使用第一个非置顶版本
		latestRegularVersion := regularVersions[0]
		latestRegularReleaseVersion := regularVersions[0]
		hasRegularRelease := !regularVersions[0].IsTest

		// 找出非置顶版本中版本号最大的
		for _, v := range regularVersions {
			regularVersionNum, _ := strconv.Atoi(v.Version)
			latestRegularVersionNum, _ := strconv.Atoi(latestRegularVersion.Version)

			if regularVersionNum > latestRegularVersionNum {
				latestRegularVersion = v
			}

			// 如果是正式版，也更新最新正式版
			if !v.IsTest {
				hasRegularRelease = true
				latestRegularReleaseVersionNum, _ := strconv.Atoi(latestRegularReleaseVersion.Version)

				if regularVersionNum > latestRegularReleaseVersionNum {
					latestRegularReleaseVersion = v
				}
			}
		}

		// 如果没有置顶版本，或者非置顶版本的版本号更大
		if latestVersion.Version == "" || (latestRegularVersion.Version != "" &&
			compareVersions(latestRegularVersion.Version, latestVersion.Version) > 0) {

			// 如果不需要跳过测试版，或者该版本不是测试版
			if !skipTest || !latestRegularVersion.IsTest {
				latestVersion = latestRegularVersion
			}
		}

		// 如果没有置顶正式版，或者非置顶正式版的版本号更大
		if hasRegularRelease && (latestReleaseVersion.Version == "" ||
			(latestRegularReleaseVersion.Version != "" &&
				compareVersions(latestRegularReleaseVersion.Version, latestReleaseVersion.Version) > 0)) {

			latestReleaseVersion = latestRegularReleaseVersion
		}
	}

	// 记录找到的最新版本
	if latestVersion.Version != "" {
		log.Printf("[API][GetDSTVersion] 最终确定的最新版本: %s (r%s), 类型: %s, 测试版: %v",
			latestVersion.Version, latestVersion.BuildNumber, latestVersion.UpdateType, latestVersion.IsTest)
	}

	if latestReleaseVersion.Version != "" {
		log.Printf("[API][GetDSTVersion] 最终确定的最新正式版: %s (r%s), 类型: %s",
			latestReleaseVersion.Version, latestReleaseVersion.BuildNumber, latestReleaseVersion.UpdateType)
	}

	// 根据参数返回相应的版本信息
	if skipTest {
		if latestReleaseVersion.Version != "" {
			c.JSON(http.StatusOK, gin.H{
				"status": 200,
				"msg":    "获取最新正式版本成功",
				"data":   latestReleaseVersion,
			})
		} else {
			c.JSON(http.StatusNotFound, gin.H{
				"status": 404,
				"msg":    "未找到正式版本信息",
			})
		}
	} else {
		if latestVersion.Version != "" {
			c.JSON(http.StatusOK, gin.H{
				"status": 200,
				"msg":    "获取最新版本成功",
				"data":   latestVersion,
			})
		} else {
			c.JSON(http.StatusNotFound, gin.H{
				"status": 404,
				"msg":    "未找到版本信息",
			})
		}
	}
}

// compareVersions 比较两个版本号的大小
// 返回值: 1 表示 v1 > v2, 0 表示 v1 = v2, -1 表示 v1 < v2
func compareVersions(v1, v2 string) int {
	v1Num, err1 := strconv.Atoi(v1)
	v2Num, err2 := strconv.Atoi(v2)

	// 如果两个版本号都能转换为数字，直接比较
	if err1 == nil && err2 == nil {
		if v1Num > v2Num {
			return 1
		} else if v1Num < v2Num {
			return -1
		} else {
			return 0
		}
	}

	// 如果不能转换为数字，则按字符串比较
	if v1 > v2 {
		return 1
	} else if v1 < v2 {
		return -1
	} else {
		return 0
	}
}
