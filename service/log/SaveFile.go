package log

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

var mutexx sync.Mutex

func check(e error) {
	if e != nil {
		panic(e)
	}
}
func checkFileExists(filename string) bool {
	var exist = true
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		exist = false
	}
	return exist

}
func Savefile(s string, mesg string) {
	defer func() {
		if err := recover(); err != nil {
			fmt.Println("panic:", err)
		}
	}()
	mutexx.Lock()
	var runtime = time.Now().Format("2006-01-02 15:04:05\r\n")
	var runtimeCompare = time.Now().Format(s + "_2006_01_02_15.serverlog")
	//fmt.Println(nowtime_compare)
	var filename = runtimeCompare
	var file *os.File
	var err error
	//每个小时一个日志文件
	if checkFileExists(filename) {
		file, err = os.OpenFile(filename, os.O_APPEND, 0666)
		fmt.Println("文件存在")
	} else {
		file, err = os.Create(filename)
		fmt.Println("文件不存在，正在创建")
	}
	check(err)
	_, err = io.WriteString(file, runtime)
	check(err)
	//fmt.Printf("写入了 %d 个字节\n", mesNum)
	_, err = io.WriteString(file, mesg)
	check(err)
	//fmt.Printf("写入了 %d 个字节\n", mesNum)
	mutexx.Unlock()
}
