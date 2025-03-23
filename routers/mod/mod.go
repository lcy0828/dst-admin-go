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
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

var wg sync.WaitGroup
var mutex sync.Mutex

var (
	//fileName = flag.String("f", "/var/serverlog/1.serverlog", "日志文件")
	fileName string
	p1       int
)

func init() {
	//fileName := "/root/Steam/logs/workshop_log.txt"
	flag.StringVar(&fileName, "f", "/root/Steam/logs/workshop_log.txt", "日志文件")
	flag.Parse()

}

type Vote struct {
	Num  string `form:"num" json:"num"`
	Star int    `form:"star" json:"num"`
}

type Searchmodinfo struct {
	Auth     string `form:"auth" json:"auth"`         //作者
	Id       string `form:"id" json:"id"`             //模组id
	Img      string `form:"img" json:"img"`           //模组图片
	Name     string `form:"name" json:"name"`         //模组名字
	Sub      string `form:"sub" json:"sub"`           //
	Time     string `form:"time" json:"time"`         //更新时间
	Version  string `form:"version" json:"version"`   //版本
	Describe string `form:"describe" json:"describe"` //版本

	Vote `form:"vote" json:"vote"`
}
type Modsearch struct {
	//token string `form:"token" json:"token" uri:"token" xml:"token" binding:"required"`
	Modname string `form:"modname" json:"modname" uri:"modname" xml:"modname" binding:"required"`
}
type Moddown struct {
	//token string `form:"token" json:"token" uri:"token" xml:"token" binding:"required"`
	Modid   string `form:"modid" json:"modid" uri:"modid" xml:"modid" binding:"required"`
	Refresh string `form:"refresh" json:"refresh" uri:"refresh" xml:"refresh" binding:"required"`
	Version string `form:"version" json:"version" uri:"version" xml:"version" binding:"required"`
}
type modResult struct {
	status  int    `json:"code"`
	modinfo string `json:"msg"`
}

func BytesToString(b []byte) string {
	return *(*string)(unsafe.Pointer(&b))
}
func SearchMod(g *gin.Context) {
	var form Modsearch

	if g.Bind(&form) == nil {
		if form.Modname == "" {
		}
	}
	var searchmodinfo [30]Searchmodinfo
	// Instantiate default collector
	c := colly.NewCollector(
		colly.AllowedDomains("steamcommunity.com"),
	)
	c.Async = true
	c.Limit(&colly.LimitRule{

		Parallelism: 1,
		RandomDelay: 1 * time.Second, // 两次请求 随机延迟5s 内
	})

	c.OnRequest(func(r *colly.Request) {
		r.Headers.Set("Accept-Language", " zh-CN,zh;q=0.9,en;q=0.8")
	})

	q := 0
	c.OnHTML("div[class=workshopItem]", func(e *colly.HTMLElement) {

		searchmodinfo[q].Id = e.ChildAttr("a", "data-publishedfileid")
		searchmodinfo[q].Img = e.ChildAttr("a>div>img", "src")
		searchurl := e.ChildAttr("a[class=ugc]", "href")
		searchmodinfo[q].Name = e.ChildText("a[class=item_link]>div")
		author := e.ChildText("div>a[class=workshop_author_link]")
		fmt.Println("模组名字:", searchmodinfo[q].Name)
		fmt.Println("模组作者:", author)
		searchmodinfo[q].Auth = author
		q++
		if searchurl != "" {
			c.Visit(searchurl)
			//que.AddURL(searchurl)
		}

	})
	w := 0
	c.OnHTML("div[class=detailsStatsContainerRight]", func(e *colly.HTMLElement) {
		fmt.Println("id1:", c.ID)
		modsize := e.ChildText("div:nth-child(1)")
		modaddtime := e.ChildText("div:nth-child(2)")
		moduptime := e.ChildText("div:nth-child(3)")
		searchmodinfo[w].Time = moduptime
		fmt.Println(modsize)
		fmt.Println(modaddtime)
		fmt.Println(moduptime)
		w++

	})
	p := 0
	c.OnHTML("table[class=stats_table]", func(e *colly.HTMLElement) {
		fmt.Println("id2:", c.ID)
		modnoresub := e.ChildText("tbody>tr:nth-child(1)>td:nth-child(1)")
		modnowsub := e.ChildText("tbody>tr:nth-child(2)>td:nth-child(1)")
		modaddsub := e.ChildText("tbody>tr:nth-child(3)>td:nth-child(1)")
		searchmodinfo[p].Sub = modnowsub
		p++
		fmt.Println(modnoresub)
		fmt.Println(modnowsub)
		fmt.Println(modaddsub)

	})
	j := 0
	c.OnHTML("div[class=workshopTags]", func(e *colly.HTMLElement) {
		modversion := e.ChildText("a")
		searchmodinfo[j].Version = modversion
		j++
		fmt.Println("modversion:")
		fmt.Println(modversion)
	})
	//l := 0
	//c.OnHTML("div[class=workshopItemDescription]", func(e *colly.HTMLElement) {
	//	//describe := e.ChildText("div[class=workshopItemDescription]")
	//	describe := e.Text
	//	searchmodinfo[l].Describe = describe
	//	l++
	//	fmt.Println("描述:", describe)
	//})

	c.OnRequest(func(r *colly.Request) {
		fmt.Println("Visiting", r.URL.String())
	})
	visurl := "https://steamcommunity.com/workshop/browse/?appid=322330&searchtext=" + form.Modname + "&browsesort=trend&section=&actualsort=trend&p=1&days=-1&numperpage=30"
	c.Visit(visurl)
	c.Wait()
	//que.Run(c)
	fmt.Println(searchmodinfo)
	g.JSON(http.StatusOK, searchmodinfo)
}

func DownloadMod(g *gin.Context) {

	var form Moddown

	if g.Bind(&form) == nil {
		if form.Modid == "" {
		}
		if form.Refresh == "" {
		}
	}
	p1 = 0
	status := 401
	fmt.Println("form:", form)
	fmt.Println("refresh值:", form.Refresh)
	modinfo := DownloadMod2(form.Modid, form.Refresh, form.Version)
	if p1 == 0 {
		status = 400
	} else if p1 == 1 {
		status = 200
	}
	if status == 401 {
		modinfo = "异常退出"
	}
	data := "{\"status\": " + strconv.Itoa(status) + ", \"modinfo\": " + modinfo + "}"
	fmt.Println(data)
	g.String(http.StatusOK, data)

}
func checktemp(modid string) string {
	ml := "cd /opt/go-dont/lua-sh/ && ls -l temp/" + modid
	checkcmd := exec.Command("bash", "-c", ml)
	_, cherr := checkcmd.CombinedOutput()
	if cherr != nil {
		//serverlog.Fatalf("checkcmd.Run() failed with %s\n", cherr)
		//return string(a1)
		//fmt.Println(a1)
		return "false"
	}
	//fmt.Println(a1)
	//return string(a1)
	return "ok"
}
func jsonmod(modid string) string {
	ml := "rm -rf /opt/go-dont/lua-sh/modinfo.lua && cp /root/Steam/steamapps/workshop/content/322330/" + modid + "/modinfo.lua /opt/go-dont/lua-sh/modinfo.lua"
	checkcmd := exec.Command("bash", "-c", ml)
	_, cherr := checkcmd.CombinedOutput()
	if cherr != nil {
		log.Fatalf("checkcmd.Run() failed with %s\n", cherr)
	}
	ml2 := "cd /opt/go-dont/lua-sh/ && lua modgetinfo.lua"
	checkcmd2 := exec.Command("bash", "-c", ml2)
	check2, cherr2 := checkcmd2.CombinedOutput()
	//fmt.Println(string(check2))
	//fmt.Println(cherr2)
	if cherr2 != nil {
		log.Fatalf("checkcmd123123.Run() failed with %s\n", cherr2)
	}
	return string(check2)

}

func DownloadMod2(modid string, refresh string, version string) string {
	var isrun bool
	isrun = true
	var form Moddown
	fmt.Println("强制刷新:", refresh)
	fmt.Println(checktemp(modid))
	if checktemp(modid) == "ok" && refresh != "true" {
		ml2 := "cd /opt/go-dont/lua-sh/temp/" + modid + "&& lua modgetinfo.lua"
		checkcmd2 := exec.Command("bash", "-c", ml2)
		check2, cherr2 := checkcmd2.CombinedOutput()
		//fmt.Println(string(check2))
		if cherr2 != nil {
		}
		fmt.Println("没有下载")
		p1 = 1
		return string(check2)
	}
	checkcmd := exec.Command("bash", "-c", "tmux has-session -t DST_MODDOWN")
	cheout, cherr := checkcmd.CombinedOutput()
	fmt.Println("输出:", string(cheout), "错误信息", cherr)
	//fmt.Println(cherr == nil)
	if cherr != nil {
		isrun = false
	}
	fmt.Println(cheout)

	fmt.Print(isrun)
	if isrun == false {
		cmd := exec.Command("bash", "-c", "cd /opt/go-dont/steam && tmux new-session -s DST_MODDOWN -d \"./steamcmd.sh\"")

		out, err := cmd.CombinedOutput()
		fmt.Printf("combined out:\n%s\n", string(out))
		if err != nil {
			log.Fatalf("cmd.Run() failed with %s\n", err)
		}
		cmd1 := exec.Command("bash", "-c", "tmux send-keys -t DST_MODDOWN 'login anonymous' C-m")
		out1, err1 := cmd1.CombinedOutput()
		fmt.Printf("combined out1:\n%s\n", string(out1))
		if err1 != nil {
			log.Fatalf("cmd1.Run() failed with %s\n", err1)
		}
	} else {
		fmt.Printf("已经存在tmux")
	}

	fmt.Println("Modid:", form.Modid)
	ml := "tmux send-keys -t DST_MODDOWN 'workshop_download_item 322330 " + modid + "' C-m"
	cmd2 := exec.Command("bash", "-c", ml)
	out2, err2 := cmd2.CombinedOutput()
	fmt.Printf("combined out2:\n%s\n", string(out2))
	if err2 != nil {
		log.Fatalf("cmd2.Run() failed with %s\n", err2)
	}
	var chan1 = make(chan string, 10)
	//wg.Add(1)
	go readmod(modid, chan1)
	//wg.Wait()
	p1 = 0
	errorresult := ""
	for {
		i, ok := <-chan1

		if i == "接收到模组下载请求" && ok {
			fmt.Println(i)
			continue
		} else if i == "模组正在下载" && ok {
			fmt.Println(i)
			continue
		} else if i == "下载成功" && ok {
			fmt.Println(i)
			fmt.Println("开始写入数据库")
			models.Updatemodversion(modid, version)
			p1 = 1
			break
		} else if i == "下载失败" {
			fmt.Println("下载失败")
			break
		} else if i == "接收到模组下载结果信息" {
			fmt.Println(i)
		} else {
			fmt.Println(i)
			errorresult = i
			break
		}
		if p1 == 1 {
			break
		}
	}
	length := len(modid) + 61
	//fmt.Println(len(errorresult))
	if p1 == 0 {
		return errorresult[length : len(errorresult)-1]
		//return "下载失败"
	}
	ml2 := "rm -rf /opt/go-dont/lua-sh/modinfo.lua && cp /root/Steam/steamapps/workshop/content/322330/" + modid + "/modinfo.lua /opt/go-dont/lua-sh/modinfo.lua && cd /opt/go-dont/lua-sh/ && mkdir -p temp/" + modid + " && cp -f modgetinfo.lua cjson.so modinfo.lua temp/" + modid + "/ && lua modgetinfo.lua"
	checkcmd2 := exec.Command("bash", "-c", ml2)
	check2, cherr2 := checkcmd2.CombinedOutput()
	//fmt.Println(string(check2))
	if cherr2 != nil {
	}
	//fmt.Println(cherr2)

	return string(check2)
}

// ////合并到search内部
//
//	func getmodextra(url string) {
//		c := colly.NewCollector(

//			colly.AllowedDomains("steamcommunity.com"),
//		)
//		c.Async = true
//		c.OnRequest(func(r *colly.Request) {
//			r.Headers.Set("Accept-Language", " zh-CN,zh;q=0.9,en;q=0.8")
//		})
//		//if p, err := proxy.RoundRobinProxySwitcher("socks5://127.0.0.1:1080", "http://127.0.0.1:1087"); err == nil {
//		//	c.SetProxyFunc(p)
//		//}
//
//		c.OnRequest(func(r *colly.Request) {
//			fmt.Println("Visiting", r.URL.String())
//		})
//		c.Visit(url)
//		c.Wait()
//		return
//	}
func readmod(modid string, ch chan string) {
	p := 0
	mod1 := "Download item " + modid + " requested by app"
	mod2 := "Starting Workshop download job (requested item " + modid + " )"
	mod3 := "Download item " + modid + " result"
	fmt.Println(mod1)
	fmt.Println(mod2)
	fmt.Println(mod3)
	tailConfig := tail.Config{
		ReOpen:    true,
		Follow:    true,
		Location:  &tail.SeekInfo{Offset: 0, Whence: 2},
		MustExist: false,
		Poll:      true,
	}

	tails, err := tail.TailFile(fileName, tailConfig)
	if err != nil {
		fmt.Println("tail.TailFile error", err)
	}

	for {
		line, ok := <-tails.Lines
		if !ok {
			fmt.Printf("tail file close reopen, filename:%s\n", tails.Filename)
			time.Sleep(time.Second)
			continue
		}
		//fmt.Print("123:", line.Text)
		if strings.Contains(line.Text, mod1) {
			//fmt.Println("接收到模组下载请求")
			ch <- "接收到模组下载请求"
			//fmt.Println("发送mod1")
		} else if strings.Contains(line.Text, mod2) {
			//fmt.Println("模组正在下载")
			ch <- "模组正在下载"
			//fmt.Println("发送mod2")
		} else if strings.Contains(line.Text, mod3) {
			//fmt.Println("接收到模组下载结果信息")
			ch <- "接收到模组下载结果信息"
			if strings.Contains(line.Text, "result : OK") {
				//fmt.Println("下载成功")
				ch <- "下载成功"
				fmt.Println("发送下载成功")
				p = 1
			} else {
				//fmt.Println("下载失败")
				ch <- line.Text
				fmt.Println("发送", line.Text)
				p = 1
			}

		}
		if p == 1 {
			//fmt.Println("结束了")
			break
		}
	}

}
