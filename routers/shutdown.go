package routers

import (
	"dont/routers/gamelog"
)

// ShutdownGameLogPositionManager 关闭 gamelog 包中的位置管理器
func ShutdownGameLogPositionManager() {
	gamelog.ShutdownPositionManager()
}
