package serverlog

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/hpcloud/tail"
	"log"
	"net/http"
	"time"
)
var upgrader = websocket.Upgrader{  CheckOrigin: func (r *http.Request) bool {  return true  },
}
func Logtailf(c *gin.Context) {
	//服务升级，对于来到的http连接进行服务升级，升级到ws
	cn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	defer cn.Close()
	if err != nil {
		panic(err)
	}
	for {
		mt, message, err := cn.ReadMessage()
		if err != nil {
			log.Println("server read:", err)
			break
		}

		log.Printf("server recv msg: %s", message)
		msg := string(message)
		if msg == "log" {

			Lslog(mt,cn)
		}
		if msg == "woshi client1" {
			message = []byte("client1 去服务端了一趟")

		} else if msg == "woshi client2" {
			message = []byte("client2 去服务端了一趟")
		}

		err = cn.WriteMessage(mt, message)
		if err != nil {
			log.Println(" server write err:", err)
			break
		}
	}
}
func Lslog(mt int,ws *websocket.Conn)  {
	fileName := "/root/DST/Klei/DoNotStarveTogether/02/Forest1/server_log.txt"
	//message := []byte(line.Text)
	config := tail.Config{
		ReOpen:    true,                                 // 重新打开
		Follow:    true,                                 // 是否跟随
		Location:  &tail.SeekInfo{Offset: 1, Whence: 2}, // 从文件的哪个地方开始读
		MustExist: false,                                // 文件不存在不报错
		Poll:      true,
	}
	tails, err := tail.TailFile(fileName, config)
	if err != nil {
		fmt.Println("tail file failed, err:", err)
		return
	}
	var (
		line *tail.Line
		ok   bool
	)
	for {
		line, ok = <-tails.Lines
		if !ok {
			fmt.Printf("tail file close reopen, filename:%s\n", tails.Filename)
			time.Sleep(time.Second)
			continue
		}
		msg:=line.Text+"\n"
		message := []byte(msg)
		ws.WriteMessage(mt, message)
		fmt.Println("line:", msg)
		//fmt.Println("line:", line.Text)
	}
}
