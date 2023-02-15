package status

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	"net/http"
	"strconv"
)

type systeminfo struct {
	Cpumod    string  `form:"cpumod" json:"cpumod"`
	Cpumhz    float64 `form:"cpumhz" json:"cpumhz"`
	Cpuload   float64 `form:"cpuload" json:"cpuload"`
	Cpuphy    int     `form:"cpuphy" json:"cpuphy"`
	Cpulog    int     `form:"cpulog" json:"cpulog"`
	Totalmem  int     `form:"totalmem" json:"totalmem"`
	Usedmem   int     `form:"usedmem" json:"usedmem"`
	Permem    string  `form:"permem" json:"permem"`
	Perdisk   float64 `form:"perdisk" json:"perdisk"`
	Totaldisk int     `form:"totaldisk" json:"totaldisk"`
	Freedisk  int     `form:"useddisk" json:"useddisk"`
}

func Cpuinfo(g *gin.Context) {
	var systeminfo systeminfo
	physicalCnt, _ := cpu.Counts(false)
	logicalCnt, _ := cpu.Counts(true)
	cpu_info, _ := cpu.Info()
	cpuload, _ := load.Avg()
	diskInfo, _ := disk.Usage("/")

	systeminfo.Perdisk = diskInfo.UsedPercent
	systeminfo.Totaldisk = int(diskInfo.Total / 1024 / 1024 / 1024)
	systeminfo.Freedisk = int(diskInfo.Free / 1024 / 1024 / 1024)
	fmt.Println("根分区已使用:", diskInfo.UsedPercent, "%")
	fmt.Println("根分区共:", diskInfo.Total/1024/1024/1024, "GB")
	fmt.Println("根分区可用:", diskInfo.Free/1024/1024/1024, "GB")

	systeminfo.Cpumod = cpu_info[0].ModelName
	systeminfo.Cpumhz = cpu_info[0].Mhz
	systeminfo.Cpuload = cpuload.Load1
	fmt.Println("cpu型号:", cpu_info[0].ModelName)
	fmt.Println("cpu频率:", cpu_info[0].Mhz)
	fmt.Println("cpu负载:", cpuload.Load1)

	v, _ := mem.VirtualMemory()
	systeminfo.Cpuphy = physicalCnt // cpu物理核数
	systeminfo.Cpulog = logicalCnt  // cpu逻辑核数
	fmt.Println("cpu物理核数:", physicalCnt)
	fmt.Println("cpu逻辑核数:", logicalCnt)

	systeminfo.Totalmem = int(v.Total / 1024 / 1024)
	systeminfo.Usedmem = int(v.Used / 1024 / 1024)
	fmt.Println("总内存：", v.Total/1024/1024)
	fmt.Println("已用内存：", v.Used/1024/1024)

	permem := strconv.FormatFloat(v.UsedPercent, 'f', 2, 64)
	systeminfo.Permem = permem
	fmt.Println("内存使用率: ", permem, "%")
	g.JSON(http.StatusOK, systeminfo)
}
