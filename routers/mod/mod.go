package mod

import (
	"dont/models"
	"flag"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/go-ini/ini"
	"github.com/gocolly/colly"
	"github.com/hpcloud/tail"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// 日志前缀常量
const (
	LogPrefix       = "[MOD]"
	LogSearchPrefix = "[MOD][SEARCH]"
	LogDownPrefix   = "[MOD][DOWNLOAD]"
	LogCachePrefix  = "[MOD][CACHE]"
	LogTmuxPrefix   = "[MOD][TMUX]"
)

// 模组相关配置变量
var (
	appID           string // 饥荒联机版的AppID
	steamCmdPath    string // Steam命令行工具路径
	luaShPath       string // Lua脚本路径
	workshopContent string // Workshop内容路径
	workshopModPath string // Workshop模组安装路径
	tmuxSessionName string // Tmux会话名称
)

var wg sync.WaitGroup
var mutex sync.Mutex

var (
	fileName string
	p1       int
)

func init() {
	// 默认值
	appID = "322330"
	steamCmdPath = "/opt/go-dont/steam"
	luaShPath = "/opt/go-dont/lua-sh"
	workshopContent = "/root/Steam/steamapps/workshop/content/322330"
	workshopModPath = "/root/Steam/"
	tmuxSessionName = "DST_MODDOWN"
	fileName = "/root/Steam/logs/workshop_log.txt"

	// 从配置文件读取
	configFile := "./conf/app.conf"
	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		if cfg, err := ini.Load(configFile); err == nil {
			// 读取模组相关配置
			modSection := cfg.Section("mod")

			if modSection.HasKey("APP_ID") {
				appID = modSection.Key("APP_ID").String()
				log.Printf("%s 从配置文件加载饥荒AppID: %s", LogPrefix, appID)
			}

			if modSection.HasKey("STEAM_CMD_PATH") {
				steamCmdPath = modSection.Key("STEAM_CMD_PATH").String()
				log.Printf("%s 从配置文件加载Steam命令行路径: %s", LogPrefix, steamCmdPath)
			}

			if modSection.HasKey("LUA_SH_PATH") {
				luaShPath = modSection.Key("LUA_SH_PATH").String()
				log.Printf("%s 从配置文件加载Lua脚本路径: %s", LogPrefix, luaShPath)
			}

			if modSection.HasKey("WORKSHOP_CONTENT") {
				workshopContent = modSection.Key("WORKSHOP_CONTENT").String()
				log.Printf("%s 从配置文件加载Workshop内容路径: %s", LogPrefix, workshopContent)
			}

			if modSection.HasKey("WORKSHOP_MOD_PATH") {
				workshopModPath = modSection.Key("WORKSHOP_MOD_PATH").String()
				log.Printf("%s 从配置文件加载Workshop模组安装路径: %s", LogPrefix, workshopModPath)
			}

			if modSection.HasKey("TMUX_SESSION_NAME") {
				tmuxSessionName = modSection.Key("TMUX_SESSION_NAME").String()
				log.Printf("%s 从配置文件加载Tmux会话名称: %s", LogPrefix, tmuxSessionName)
			}

			if modSection.HasKey("WORKSHOP_LOG_FILE") {
				// 处理路径中的转义字符
				fileName = strings.ReplaceAll(modSection.Key("WORKSHOP_LOG_FILE").String(), "\\", "")
				log.Printf("%s 从配置文件加载模组下载日志文件: %s", LogPrefix, fileName)
			}
		} else {
			log.Printf("%s 警告: 无法读取配置文件: %v, 使用默认配置", LogPrefix, err)
		}
	} else {
		log.Printf("%s 警告: 配置文件不存在, 使用默认配置", LogPrefix)
	}

	// 命令行参数优先级更高
	flag.StringVar(&fileName, "f", fileName, "日志文件")
}

// Vote 模组评分结构
type Vote struct {
	Num  string `form:"num" json:"num"`
	Star int    `form:"star" json:"star"`
}

// Searchmodinfo 模组搜索结果信息
type Searchmodinfo struct {
	Auth      string `form:"auth" json:"auth"`             // 作者
	Id        string `form:"id" json:"id"`                 // 模组id
	Img       string `form:"img" json:"img"`               // 模组图片
	Name      string `form:"name" json:"name"`             // 模组名字
	Sub       string `form:"sub" json:"sub"`               // 订阅数
	Time      string `form:"time" json:"time"`             // 更新时间
	Version   string `form:"version" json:"version"`       // 版本
	Describe  string `form:"describe" json:"describe"`     // 描述
	RatingImg string `form:"rating_img" json:"rating_img"` // 评分图片

	Vote `form:"vote" json:"vote"`
}

// Modsearch 模组搜索请求参数
type Modsearch struct {
	Modname string `form:"modname" json:"modname" uri:"modname" xml:"modname" binding:"required"`
}

// Moddown 模组下载请求参数
type Moddown struct {
	Modid   string `form:"modid" json:"modid" uri:"modid" xml:"modid" binding:"required"`
	Refresh string `form:"refresh" json:"refresh" uri:"refresh" xml:"refresh" binding:"required"`
	Version string `form:"version" json:"version" uri:"version" xml:"version" binding:"required"`
}

// APIResponse 标准API响应结构
type APIResponse struct {
	Status int    `json:"status"`
	Info   string `json:"modinfo"`
}

// BytesToString 字节数组转字符串
func BytesToString(b []byte) string {
	return *(*string)(unsafe.Pointer(&b))
}

// SearchMod 搜索模组处理函数
func SearchMod(g *gin.Context) {
	startTime := time.Now()
	clientIP := g.ClientIP()
	log.Printf("%s 收到模组搜索请求 来自IP: %s", LogSearchPrefix, clientIP)

	keyword := g.Query("keyword")
	page := g.DefaultQuery("page", "1")

	// 如果没有提供关键字，则尝试从表单或JSON中获取
	if keyword == "" {
		var form Modsearch
		if err := g.Bind(&form); err == nil && form.Modname != "" {
			keyword = form.Modname
			log.Printf("%s 从请求体获取到关键字: %s", LogSearchPrefix, keyword)
		} else if err != nil {
			log.Printf("%s 请求体解析失败: %v", LogSearchPrefix, err)
		}
	} else {
		log.Printf("%s 从查询参数获取到关键字: %s, 页码: %s", LogSearchPrefix, keyword, page)
	}

	// 检查是否有关键字
	if keyword == "" {
		log.Printf("%s 未提供搜索关键字 IP: %s", LogSearchPrefix, clientIP)
		g.JSON(http.StatusBadRequest, gin.H{"error": "请提供搜索关键字"})
		return
	}

	// 使用map存储模组信息，以模组ID为键
	modInfoMap := make(map[string]*Searchmodinfo)
	var modList []string // 保持模组顺序

	// 互斥锁用于保护map的并发访问
	var mapMutex sync.Mutex

	// 初始化爬虫
	c := colly.NewCollector(
		colly.AllowedDomains("steamcommunity.com"),
	)
	c.Async = true
	c.Limit(&colly.LimitRule{
		Parallelism: 2, // 增加并行度提高爬取速度
		RandomDelay: 1 * time.Second,
	})

	c.OnRequest(func(r *colly.Request) {
		r.Headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		log.Printf("%s 正在访问: %s", LogSearchPrefix, r.URL.String())
	})

	c.OnError(func(r *colly.Response, err error) {
		log.Printf("%s 爬取错误: %v URL: %s 状态码: %d",
			LogSearchPrefix, err, r.Request.URL, r.StatusCode)
	})

	// 解析模组基础信息
	c.OnHTML("div[class=workshopItem]", func(e *colly.HTMLElement) {
		modID := e.ChildAttr("a", "data-publishedfileid")
		if modID == "" {
			return
		}

		mapMutex.Lock()

		// 检查是否已存在，如果不存在则创建新项
		if _, exists := modInfoMap[modID]; !exists {
			modInfoMap[modID] = &Searchmodinfo{Id: modID}
			modList = append(modList, modID) // 保持顺序
		}

		// 更新模组基本信息
		modInfoMap[modID].Img = e.ChildAttr("a>div>img", "src")
		modInfoMap[modID].Name = e.ChildText("a[class=item_link]>div")
		modInfoMap[modID].Auth = e.ChildText("div>a[class=workshop_author_link]")

		// 提取评分图片
		ratingImg := e.ChildAttr("img.fileRating", "src")
		if ratingImg == "" {
			// 尝试另一种选择器格式
			ratingImg = e.ChildAttr("img[class=fileRating]", "src")
		}
		// 如果仍然找不到，尝试更宽泛的搜索
		if ratingImg == "" {
			e.ForEach("img", func(_ int, img *colly.HTMLElement) {
				if img.Attr("class") == "fileRating" {
					ratingImg = img.Attr("src")
				}
			})
		}
		if ratingImg != "" {
			modInfoMap[modID].RatingImg = ratingImg
			log.Printf("模组 %s 评分图片: %s", modID, ratingImg)
		}

		mapMutex.Unlock()

		log.Printf("找到模组: %s (ID: %s, 作者: %s)",
			modInfoMap[modID].Name, modID, modInfoMap[modID].Auth)

		// 访问详情页获取更多信息
		detailURL := e.ChildAttr("a[class=ugc]", "href")
		if detailURL != "" {
			// 将模组ID作为上下文传递给详情页爬取
			detailCtx := colly.NewContext()
			detailCtx.Put("modID", modID)
			c.Request("GET", detailURL, nil, detailCtx, nil)
		}
	})

	// 解析模组详细统计信息
	c.OnHTML("div[class=detailsStatsContainerRight]", func(e *colly.HTMLElement) {
		// 从上下文获取模组ID
		modID := e.Request.Ctx.Get("modID")
		if modID == "" {
			return
		}

		mapMutex.Lock()
		defer mapMutex.Unlock()

		if _, exists := modInfoMap[modID]; !exists {
			// 如果模组ID不存在，可能是爬虫直接访问了详情页
			return
		}

		// 获取并设置模组时间信息
		moduptime := e.ChildText("div:nth-child(3)")
		if moduptime != "" {
			modInfoMap[modID].Time = moduptime
			log.Printf("模组 %s 更新时间: %s", modID, moduptime)
		}
	})

	// 解析模组订阅信息
	c.OnHTML("table[class=stats_table]", func(e *colly.HTMLElement) {
		modID := e.Request.Ctx.Get("modID")
		if modID == "" {
			return
		}

		mapMutex.Lock()
		defer mapMutex.Unlock()

		if _, exists := modInfoMap[modID]; !exists {
			return
		}

		// 获取并设置订阅信息
		modnowsub := e.ChildText("tbody>tr:nth-child(2)>td:nth-child(1)")
		if modnowsub != "" {
			modInfoMap[modID].Sub = modnowsub
			log.Printf("模组 %s 订阅数: %s", modID, modnowsub)
		}
	})

	// 解析模组版本信息
	c.OnHTML("div[class=workshopTags]", func(e *colly.HTMLElement) {
		modID := e.Request.Ctx.Get("modID")
		if modID == "" {
			return
		}

		mapMutex.Lock()
		defer mapMutex.Unlock()

		if _, exists := modInfoMap[modID]; !exists {
			return
		}

		// 获取并设置版本信息
		modversion := e.ChildText("a")
		if modversion != "" {
			modInfoMap[modID].Version = modversion
			log.Printf("模组 %s 版本: %s", modID, modversion)
		}
	})

	// 访问搜索页面
	searchURL := fmt.Sprintf(
		"https://steamcommunity.com/workshop/browse/?appid=%s&searchtext=%s&browsesort=trend&section=&actualsort=trend&p=%s&days=-1&numperpage=30",
		appID, keyword, page,
	)

	log.Printf("%s 开始访问搜索页面: %s", LogSearchPrefix, searchURL)
	if err := c.Visit(searchURL); err != nil {
		log.Printf("%s 搜索失败: %v URL: %s", LogSearchPrefix, err, searchURL)
		g.JSON(http.StatusInternalServerError, gin.H{"error": "搜索失败"})
		return
	}

	log.Printf("%s 等待爬取完成...", LogSearchPrefix)
	c.Wait()

	// 按原始顺序构建结果数组
	var searchResults [30]Searchmodinfo
	for i, modID := range modList {
		if i >= 30 {
			log.Printf("%s 结果超过30个，只返回前30个", LogSearchPrefix)
			break // 最多返回30个结果
		}
		if info, exists := modInfoMap[modID]; exists {
			searchResults[i] = *info
		}
	}

	elapsedTime := time.Since(startTime)
	log.Printf("%s 搜索完成，关键字: %s, 页码: %s, 找到: %d 个模组, 耗时: %v",
		LogSearchPrefix, keyword, page, len(modList), elapsedTime)

	g.JSON(http.StatusOK, searchResults)
}

// DownloadMod 下载模组处理函数
func DownloadMod(g *gin.Context) {
	startTime := time.Now()
	clientIP := g.ClientIP()
	log.Printf("%s 收到模组下载请求 来自IP: %s", LogDownPrefix, clientIP)

	modid := g.Query("modid")
	refresh := g.DefaultQuery("refresh", "false")
	version := g.DefaultQuery("version", "")

	// 如果没有从查询参数获取到，则尝试从表单或JSON中获取
	if modid == "" {
		var form Moddown
		if err := g.Bind(&form); err == nil && form.Modid != "" {
			modid = form.Modid
			refresh = form.Refresh
			version = form.Version
			log.Printf("%s 从请求体获取到参数 - 模组ID: %s, 刷新: %s, 版本: %s",
				LogDownPrefix, modid, refresh, version)
		} else if err != nil {
			log.Printf("%s 请求体解析失败: %v", LogDownPrefix, err)
		}
	} else {
		log.Printf("%s 从查询参数获取到参数 - 模组ID: %s, 刷新: %s, 版本: %s",
			LogDownPrefix, modid, refresh, version)
	}

	// 检查是否有模组ID
	if modid == "" {
		log.Printf("%s 未提供模组ID IP: %s", LogDownPrefix, clientIP)
		g.JSON(http.StatusBadRequest, gin.H{"error": "请提供模组ID"})
		return
	}

	p1 = 0
	status := 401
	log.Printf("%s 开始处理模组下载请求 - 模组ID: %s, 刷新: %s, 版本: %s",
		LogDownPrefix, modid, refresh, version)

	log.Printf("%s 调用DownloadMod2开始下载模组 - 模组ID: %s", LogDownPrefix, modid)
	modinfo := DownloadMod2(modid, refresh, version)

	switch p1 {
	case 0:
		status = 400
		log.Printf("%s 模组下载失败 - 模组ID: %s, 状态码: %d", LogDownPrefix, modid, status)
	case 1:
		status = 200
		log.Printf("%s 模组下载成功 - 模组ID: %s, 状态码: %d", LogDownPrefix, modid, status)
	default:
		status = 401
		modinfo = "异常退出"
		log.Printf("%s 模组下载异常 - 模组ID: %s, 状态码: %d", LogDownPrefix, modid, status)
	}

	// 直接构建JSON字符串，避免嵌套JSON被转义
	data := fmt.Sprintf("{\"status\": %d, \"modinfo\": %s}", status, modinfo)

	elapsedTime := time.Since(startTime)
	log.Printf("%s 模组下载请求处理完成 - 模组ID: %s, 状态码: %d, 耗时: %v",
		LogDownPrefix, modid, status, elapsedTime)

	g.Header("Content-Type", "application/json")
	g.String(http.StatusOK, data)
}

// checktemp 检查模组缓存是否存在
func checktemp(modid string) string {
	log.Printf("%s 检查模组缓存 - 模组ID: %s", LogCachePrefix, modid)
	cmd := fmt.Sprintf("cd %s && ls -l temp/%s", luaShPath, modid)
	output, err := exec.Command("bash", "-c", cmd).CombinedOutput()

	if err != nil {
		log.Printf("%s 模组缓存不存在 - 模组ID: %s, 错误: %v", LogCachePrefix, modid, err)
		return "false"
	}

	log.Printf("%s 模组缓存存在 - 模组ID: %s, 输出: %s", LogCachePrefix, modid, strings.TrimSpace(string(output)))
	return "ok"
}

// jsonmod 解析模组信息并转换为JSON格式
func jsonmod(modid string) string {
	log.Printf("%s 开始解析模组信息 - 模组ID: %s", LogPrefix, modid)

	// 复制模组信息到处理目录
	copyCmd := fmt.Sprintf("rm -rf %s/modinfo.lua && cp %s/%s/modinfo.lua %s/modinfo.lua",
		luaShPath, workshopContent, modid, luaShPath)

	output, err := exec.Command("bash", "-c", copyCmd).CombinedOutput()
	if err != nil {
		log.Printf("%s 复制模组信息失败 - 模组ID: %s, 错误: %v, 输出: %s",
			LogPrefix, modid, err, strings.TrimSpace(string(output)))
		return "{\"error\": \"复制模组信息失败\"}"
	}

	// 使用Lua脚本解析模组信息
	parseCmd := fmt.Sprintf("cd %s && lua modgetinfo.lua", luaShPath)
	output, err = exec.Command("bash", "-c", parseCmd).CombinedOutput()

	if err != nil {
		log.Printf("%s 解析模组信息失败 - 模组ID: %s, 错误: %v, 输出: %s",
			LogPrefix, modid, err, strings.TrimSpace(string(output)))
		return "{\"error\": \"解析模组信息失败\"}"
	}

	log.Printf("%s 模组信息解析成功 - 模组ID: %s", LogPrefix, modid)
	return string(output)
}

// DownloadMod2 处理模组下载的具体实现
func DownloadMod2(modid string, refresh string, version string) string {
	startTime := time.Now()
	log.Printf("%s 开始处理模组下载 - 模组ID: %s, 强制刷新: %s, 版本: %s",
		LogDownPrefix, modid, refresh, version)

	// 检查缓存
	if checktemp(modid) == "ok" && refresh != "true" {
		log.Printf("%s 使用缓存的模组信息 - 模组ID: %s", LogCachePrefix, modid)
		cmd := fmt.Sprintf("cd %s/temp/%s && lua modgetinfo.lua", luaShPath, modid)
		output, err := exec.Command("bash", "-c", cmd).CombinedOutput()

		if err == nil {
			p1 = 1
			log.Printf("%s 从缓存读取模组信息成功 - 模组ID: %s, 耗时: %v",
				LogCachePrefix, modid, time.Since(startTime))
			return string(output)
		}

		log.Printf("%s 读取缓存失败 - 模组ID: %s, 错误: %v, 输出: %s",
			LogCachePrefix, modid, err, strings.TrimSpace(string(output)))
	}

	// 检查tmux会话是否存在
	log.Printf("%s 检查tmux会话是否存在 - 会话名: %s", LogTmuxPrefix, tmuxSessionName)
	var isSessionExists bool
	checkCmd := exec.Command("bash", "-c", fmt.Sprintf("tmux has-session -t %s", tmuxSessionName))

	if err := checkCmd.Run(); err != nil {
		isSessionExists = false
		log.Printf("%s tmux会话不存在 - 会话名: %s, 错误: %v", LogTmuxPrefix, tmuxSessionName, err)
	} else {
		isSessionExists = true
		log.Printf("%s tmux会话已存在 - 会话名: %s", LogTmuxPrefix, tmuxSessionName)
	}

	// 如果会话不存在，创建新会话
	if !isSessionExists {
		initCmd := fmt.Sprintf("cd %s && tmux new-session -s %s -d \"./steamcmd.sh\"",
			steamCmdPath, tmuxSessionName)
		log.Printf("%s 创建tmux会话 - 命令: %s", LogTmuxPrefix, initCmd)

		cmd := exec.Command("bash", "-c", initCmd)
		output, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("%s 创建tmux会话失败 - 会话名: %s, 错误: %v, 输出: %s",
				LogTmuxPrefix, tmuxSessionName, err, strings.TrimSpace(string(output)))
			return "创建会话失败"
		}
		log.Printf("%s tmux会话创建成功 - 会话名: %s", LogTmuxPrefix, tmuxSessionName)

		// 设置模组安装路径
		setPathCmd := fmt.Sprintf("tmux send-keys -t %s 'force_install_dir %s' C-m", tmuxSessionName, workshopModPath)
		log.Printf("%s 发送设置模组安装路径命令 - 会话名: %s, 路径: %s", LogTmuxPrefix, tmuxSessionName, workshopModPath)

		cmd = exec.Command("bash", "-c", setPathCmd)
		output, err = cmd.CombinedOutput()
		if err != nil {
			log.Printf("%s 设置模组安装路径失败 - 会话名: %s, 错误: %v, 输出: %s",
				LogTmuxPrefix, tmuxSessionName, err, strings.TrimSpace(string(output)))
			return "设置模组安装路径失败"
		}
		log.Printf("%s 设置模组安装路径命令发送成功 - 会话名: %s", LogTmuxPrefix, tmuxSessionName)

		// 登录Steam
		loginCmd := fmt.Sprintf("tmux send-keys -t %s 'login anonymous' C-m", tmuxSessionName)
		log.Printf("%s 发送Steam匿名登录命令 - 会话名: %s", LogTmuxPrefix, tmuxSessionName)

		cmd = exec.Command("bash", "-c", loginCmd)
		output, err = cmd.CombinedOutput()
		if err != nil {
			log.Printf("%s Steam登录失败 - 会话名: %s, 错误: %v, 输出: %s",
				LogTmuxPrefix, tmuxSessionName, err, strings.TrimSpace(string(output)))
			return "Steam登录失败"
		}
		log.Printf("%s Steam登录命令发送成功 - 会话名: %s", LogTmuxPrefix, tmuxSessionName)
	}

	// 发送下载命令
	downloadCmd := fmt.Sprintf("tmux send-keys -t %s 'workshop_download_item %s %s' C-m",
		tmuxSessionName, appID, modid)
	log.Printf("%s 发送模组下载命令 - 会话名: %s, 模组ID: %s",
		LogTmuxPrefix, tmuxSessionName, modid)

	cmd := exec.Command("bash", "-c", downloadCmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("%s 发送下载命令失败 - 会话名: %s, 模组ID: %s, 错误: %v, 输出: %s",
			LogTmuxPrefix, tmuxSessionName, modid, err, strings.TrimSpace(string(output)))
		return "发送下载命令失败"
	}
	log.Printf("%s 下载命令发送成功 - 会话名: %s, 模组ID: %s",
		LogTmuxPrefix, tmuxSessionName, modid)

	// 创建通道监听下载进度
	log.Printf("%s 开始监听模组下载进度 - 模组ID: %s", LogDownPrefix, modid)
	downloadStatusChan := make(chan string, 10)
	go readmod(modid, downloadStatusChan)

	p1 = 0
	errorResult := ""

	// 处理下载状态
	for statusMsg := range downloadStatusChan {
		log.Printf("%s 模组下载状态 - 模组ID: %s, 状态: %s", LogDownPrefix, modid, statusMsg)

		switch statusMsg {
		case "接收到模组下载请求", "模组正在下载", "接收到模组下载结果信息":
			// 这些是中间状态，继续等待
			continue
		case "下载成功":
			log.Printf("%s 模组下载成功，开始更新数据库 - 模组ID: %s, 版本: %s",
				LogDownPrefix, modid, version)

			// 更新数据库中的模组版本信息
			success := models.Updatemodversion(modid, version)
			if !success {
				log.Printf("%s 更新模组版本信息失败 - 模组ID: %s, 版本: %s",
					LogDownPrefix, modid, version)
			} else {
				log.Printf("%s 更新模组版本信息成功 - 模组ID: %s, 版本: %s",
					LogDownPrefix, modid, version)
			}

			p1 = 1
			close(downloadStatusChan)
			break
		case "下载失败":
			log.Printf("%s 模组下载失败 - 模组ID: %s", LogDownPrefix, modid)
			close(downloadStatusChan)
			break
		default:
			// 其他状态消息可能包含错误信息
			errorResult = statusMsg
			log.Printf("%s 模组下载返回未知状态 - 模组ID: %s, 状态: %s",
				LogDownPrefix, modid, statusMsg)
			close(downloadStatusChan)
			break
		}

		if p1 == 1 {
			log.Printf("%s 模组下载处理完成 - 模组ID: %s, 状态: 成功", LogDownPrefix, modid)
			break
		}
	}

	// 下载失败处理
	if p1 == 0 && errorResult != "" {
		length := len(modid) + 61
		if len(errorResult) > length {
			errorResult = errorResult[length : len(errorResult)-1]
			log.Printf("%s 模组下载失败，返回错误信息 - 模组ID: %s, 错误: %s",
				LogDownPrefix, modid, errorResult)
			return errorResult
		}
		log.Printf("%s 模组下载失败 - 模组ID: %s", LogDownPrefix, modid)
		return "下载失败"
	}

	// 处理下载成功的模组信息
	log.Printf("%s 模组下载成功，开始创建缓存 - 模组ID: %s", LogCachePrefix, modid)
	createCacheCmd := fmt.Sprintf("rm -rf %s/modinfo.lua && cp %s/%s/modinfo.lua %s/modinfo.lua && cd %s && mkdir -p temp/%s && cp -f modgetinfo.lua cjson.so modinfo.lua temp/%s/ && lua modgetinfo.lua",
		luaShPath, workshopContent, modid, luaShPath, luaShPath, modid, modid)

	cmd = exec.Command("bash", "-c", createCacheCmd)
	var cmdOutput []byte
	cmdOutput, err = cmd.CombinedOutput()
	if err != nil {
		log.Printf("%s 创建模组缓存失败 - 模组ID: %s, 错误: %v",
			LogCachePrefix, modid, err)
	} else {
		log.Printf("%s 创建模组缓存成功 - 模组ID: %s", LogCachePrefix, modid)
	}

	return string(cmdOutput)
}

// readmod 监控模组下载进度
func readmod(modid string, ch chan string) {
	downloadComplete := false
	startTime := time.Now()

	// 定义需要监控的日志关键字
	mod1 := "Download item " + modid + " requested by app"
	mod2 := "Starting Workshop download job (requested item " + modid + " )"
	mod3 := "Download item " + modid + " result"

	log.Printf("%s 开始监控模组下载日志 - 模组ID: %s", LogDownPrefix, modid)

	// 配置tail
	tailConfig := tail.Config{
		ReOpen:    true,
		Follow:    true,
		Location:  &tail.SeekInfo{Offset: 0, Whence: 2},
		MustExist: false,
		Poll:      true,
	}

	// 开始tail监控日志文件
	//fileName := steamCmdPath + "/logs/stderr.txt"

	// 确保路径中的空格被正确处理
	cleanPath := strings.ReplaceAll(fileName, "\\", "")
	log.Printf("%s 开始监控日志文件 - 模组ID: %s, 文件路径: %s", LogDownPrefix, modid, cleanPath)

	tails, err := tail.TailFile(cleanPath, tailConfig)
	if err != nil {
		log.Printf("%s 监控日志文件失败 - 模组ID: %s, 错误: %v", LogDownPrefix, modid, err)
		ch <- "下载失败"
		return
	}
	log.Printf("%s 日志文件监控已启动 - 模组ID: %s", LogDownPrefix, modid)

	// 设置超时保护
	timeout := time.After(5 * time.Minute)
	log.Printf("%s 设置下载超时时间为5分钟 - 模组ID: %s", LogDownPrefix, modid)

	// 监控日志
	for {
		select {
		case <-timeout:
			log.Printf("%s 模组下载超时 - 模组ID: %s, 超时时间: 5分钟", LogDownPrefix, modid)
			ch <- "下载失败，超时"
			return
		case line, ok := <-tails.Lines:
			if !ok {
				log.Printf("%s 日志文件关闭，尝试重新打开 - 模组ID: %s, 文件: %s",
					LogDownPrefix, modid, tails.Filename)
				time.Sleep(time.Second)
				continue
			}

			if strings.Contains(line.Text, mod1) {
				log.Printf("%s 检测到模组下载请求 - 模组ID: %s", LogDownPrefix, modid)
				ch <- "接收到模组下载请求"
			} else if strings.Contains(line.Text, mod2) {
				log.Printf("%s 检测到模组开始下载 - 模组ID: %s", LogDownPrefix, modid)
				ch <- "模组正在下载"
			} else if strings.Contains(line.Text, mod3) {
				log.Printf("%s 检测到模组下载结果 - 模组ID: %s", LogDownPrefix, modid)
				ch <- "接收到模组下载结果信息"
				if strings.Contains(line.Text, "result : OK") {
					log.Printf("%s 模组下载成功 - 模组ID: %s, 耗时: %v",
						LogDownPrefix, modid, time.Since(startTime))
					ch <- "下载成功"
					downloadComplete = true
				} else {
					log.Printf("%s 模组下载失败 - 模组ID: %s, 错误日志: %s",
						LogDownPrefix, modid, line.Text)
					ch <- line.Text
					downloadComplete = true
				}
			}

			if downloadComplete {
				log.Printf("%s 模组下载过程完成 - 模组ID: %s, 耗时: %v",
					LogDownPrefix, modid, time.Since(startTime))
				return
			}
		}
	}
}
