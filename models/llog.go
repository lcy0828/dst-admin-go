package models

import (
	"dont/service/log"
	"fmt"
	"sync"
)

var wg sync.WaitGroup

func SaverFiletest() {
	//var logg = serverlog.LogMes("")
	//logg = "现在呢"
	//logg.INFO()

	//并发买票和测试
	//serverlog.Pay()
	//serverlog.Paytest()

	//并发测试
	wg.Add(4)
	go logsavetest()
	go logsavetest()
	go logsavetest()
	go logsavetest()
	wg.Wait()
}

func logsavetest() {
	defer wg.Done()
	for i := 0; i < 100; i++ {
		s := fmt.Sprintf("现在i是%d", i)
		logg := log.LogMes(s)
		logg.INFO()
		logg.DEBUG()
		logg.ERROR()
		logg.WARN()
	}
}
