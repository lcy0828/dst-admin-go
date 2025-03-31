package mod

import (
	"dont/models"
	"flag"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/gocolly/colly"
	"github.com/hpcloud/tail"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// 配置常量
const (
	appID             = "322330" // 饥荒联机版的AppID
	steamCmdPath      = "/opt/go-dont/steam"
	luaShPath         = "/opt/go-dont/lua-sh"
	workshopContent   = "/root/Steam/steamapps/workshop/content/322330"
	tmuxSessionName   = "DST_MODDOWN"
)

var wg sync.WaitGroup
var mutex sync.Mutex

var (
	fileName string
	p1       int
)

func init() {
	flag.StringVar(&fileName, "f", "/root/Steam/logs/workshop_log.txt", "日志文件")
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
	keyword := g.Query("keyword")
	page := g.DefaultQuery("page", "1")

	// 如果没有提供关键字，则尝试从表单或JSON中获取
	if keyword == "" {
		var form Modsearch
		if err := g.Bind(&form); err == nil && form.Modname != "" {
			keyword = form.Modname
		}
	}

	// 检查是否有关键字
	if keyword == "" {
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
		log.Println("正在访问:", r.URL.String())
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

	if err := c.Visit(searchURL); err != nil {
		g.JSON(http.StatusInternalServerError, gin.H{"error": "搜索失败"})
		return
	}
	
	c.Wait()
	
	// 按原始顺序构建结果数组
	var searchResults [30]Searchmodinfo
	for i, modID := range modList {
		if i >= 30 {
			break // 最多返回30个结果
		}
		if info, exists := modInfoMap[modID]; exists {
			searchResults[i] = *info
		}
	}
	
	g.JSON(http.StatusOK, searchResults)
}

// DownloadMod 下载模组处理函数
func DownloadMod(g *gin.Context) {
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
		}
	}

	// 检查是否有模组ID
	if modid == "" {
		g.JSON(http.StatusBadRequest, gin.H{"error": "请提供模组ID"})
		return
	}

	p1 = 0
	status := 401
	log.Printf("收到模组下载请求: ID=%s, 刷新=%s, 版本=%s", modid, refresh, version)

	modinfo := DownloadMod2(modid, refresh, version)

	switch p1 {
	case 0:
		status = 400
	case 1:
		status = 200
	default:
		status = 401
		modinfo = "异常退出"
	}

	// 直接构建JSON字符串，避免嵌套JSON被转义
	data := fmt.Sprintf("{\"status\": %d, \"modinfo\": %s}", status, modinfo)
	
	g.Header("Content-Type", "application/json")
	g.String(http.StatusOK, data)
}

func checktemp(modid string) string {
	cmd := fmt.Sprintf("cd %s && ls -l temp/%s", luaShPath, modid)
	if _, err := exec.Command("bash", "-c", cmd).CombinedOutput(); err != nil {
		return "false"
	}
	return "ok"
}

func jsonmod(modid string) string {
	// 复制模组信息到处理目录
	copyCmd := fmt.Sprintf("rm -rf %s/modinfo.lua && cp %s/%s/modinfo.lua %s/modinfo.lua",
		luaShPath, workshopContent, modid, luaShPath)

	if _, err := exec.Command("bash", "-c", copyCmd).CombinedOutput(); err != nil {
		log.Printf("复制模组信息失败: %v", err)
		return "{\"error\": \"复制模组信息失败\"}"
	}

	// 使用Lua脚本解析模组信息
	parseCmd := fmt.Sprintf("cd %s && lua modgetinfo.lua", luaShPath)
	output, err := exec.Command("bash", "-c", parseCmd).CombinedOutput()

	if err != nil {
		log.Printf("解析模组信息失败: %v", err)
		return "{\"error\": \"解析模组信息失败\"}"
	}

	return string(output)
}

func DownloadMod2(modid string, refresh string, version string) string {
	log.Printf("处理模组下载: ID=%s, 强制刷新=%s", modid, refresh)

	// 检查缓存
	if checktemp(modid) == "ok" && refresh != "true" {
		log.Println("使用缓存的模组信息")
		cmd := fmt.Sprintf("cd %s/temp/%s && lua modgetinfo.lua", luaShPath, modid)
		output, err := exec.Command("bash", "-c", cmd).CombinedOutput()

		if err == nil {
			p1 = 1
			return string(output)
		}

		log.Printf("读取缓存失败: %v", err)
	}

	// 检查tmux会话是否存在
	var isSessionExists bool
	checkCmd := exec.Command("bash", "-c", fmt.Sprintf("tmux has-session -t %s", tmuxSessionName))

	if err := checkCmd.Run(); err != nil {
		isSessionExists = false
	} else {
		isSessionExists = true
		log.Println("已存在tmux会话")
	}

	// 如果会话不存在，创建新会话
	if !isSessionExists {
		initCmd := fmt.Sprintf("cd %s && tmux new-session -s %s -d \"./steamcmd.sh\"",
			steamCmdPath, tmuxSessionName)

		cmd := exec.Command("bash", "-c", initCmd)
		if err := cmd.Run(); err != nil {
			log.Printf("创建tmux会话失败: %v", err)
			return "创建会话失败"
		}

		// 登录Steam
		loginCmd := fmt.Sprintf("tmux send-keys -t %s 'login anonymous' C-m", tmuxSessionName)
		cmd = exec.Command("bash", "-c", loginCmd)
		if err := cmd.Run(); err != nil {
			log.Printf("Steam登录失败: %v", err)
			return "Steam登录失败"
		}
	}

	// 发送下载命令
	downloadCmd := fmt.Sprintf("tmux send-keys -t %s 'workshop_download_item %s %s' C-m",
		tmuxSessionName, appID, modid)

	cmd := exec.Command("bash", "-c", downloadCmd)
	if err := cmd.Run(); err != nil {
		log.Printf("发送下载命令失败: %v", err)
		return "发送下载命令失败"
	}

	// 创建通道监听下载进度
	downloadStatusChan := make(chan string, 10)
	go readmod(modid, downloadStatusChan)

	p1 = 0
	errorResult := ""

	// 处理下载状态
	for statusMsg := range downloadStatusChan {
		log.Printf("下载状态: %s", statusMsg)

		switch statusMsg {
		case "接收到模组下载请求", "模组正在下载", "接收到模组下载结果信息":
			continue
		case "下载成功":
			log.Println("模组下载成功，开始更新数据库")
			models.Updatemodversion(modid, version)
			p1 = 1
			close(downloadStatusChan)
			break
		case "下载失败":
			log.Println("模组下载失败")
			close(downloadStatusChan)
			break
		default:
			errorResult = statusMsg
			close(downloadStatusChan)
			break
		}

		if p1 == 1 {
			break
		}
	}

	// 下载失败处理
	if p1 == 0 && errorResult != "" {
		length := len(modid) + 61
		if len(errorResult) > length {
			return errorResult[length : len(errorResult)-1]
		}
		return "下载失败"
	}

	// 处理下载成功的模组信息
	createCacheCmd := fmt.Sprintf("rm -rf %s/modinfo.lua && cp %s/%s/modinfo.lua %s/modinfo.lua && cd %s && mkdir -p temp/%s && cp -f modgetinfo.lua cjson.so modinfo.lua temp/%s/ && lua modgetinfo.lua",
		luaShPath, workshopContent, modid, luaShPath, luaShPath, modid, modid)

	cmd = exec.Command("bash", "-c", createCacheCmd)
	output, _ := cmd.CombinedOutput()

	return string(output)
}

func readmod(modid string, ch chan string) {
	downloadComplete := false

	// 定义需要监控的日志关键字
	mod1 := "Download item " + modid + " requested by app"
	mod2 := "Starting Workshop download job (requested item " + modid + " )"
	mod3 := "Download item " + modid + " result"

	log.Printf("开始监控模组 %s 的下载日志", modid)

	// 配置tail
	tailConfig := tail.Config{
		ReOpen:    true,
		Follow:    true,
		Location:  &tail.SeekInfo{Offset: 0, Whence: 2},
		MustExist: false,
		Poll:      true,
	}

	tails, err := tail.TailFile(fileName, tailConfig)
	if err != nil {
		log.Printf("监控日志文件失败: %v", err)
		ch <- "下载失败"
		return
	}

	// 设置超时保护
	timeout := time.After(5 * time.Minute)

	// 监控日志
	for {
		select {
		case <-timeout:
			log.Printf("下载模组 %s 超时", modid)
			ch <- "下载失败，超时"
			return
		case line, ok := <-tails.Lines:
			if !ok {
				log.Printf("日志文件关闭，尝试重新打开: %s", tails.Filename)
				time.Sleep(time.Second)
				continue
			}

			if strings.Contains(line.Text, mod1) {
				ch <- "接收到模组下载请求"
			} else if strings.Contains(line.Text, mod2) {
				ch <- "模组正在下载"
			} else if strings.Contains(line.Text, mod3) {
				ch <- "接收到模组下载结果信息"
				if strings.Contains(line.Text, "result : OK") {
					ch <- "下载成功"
					downloadComplete = true
				} else {
					ch <- line.Text
					downloadComplete = true
				}
			}

			if downloadComplete {
				log.Printf("模组 %s 下载过程完成", modid)
				return
			}
		}
	}
}
