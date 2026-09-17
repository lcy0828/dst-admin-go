package installationlock

import (
	"errors"
	"os"
	"path/filepath"
)

var ErrBusy = errors.New("该 DST 安装正在运行世界、更新游戏或安装 LuaJIT，请稍后重试")

const Filename = ".dst-admin-luajit.lock"
const Transaction = ".dst-admin-luajit-transaction"

func Acquire(root string) (func(), error) {
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, err
	}
	return exclusive(filepath.Join(root, Filename))
}
