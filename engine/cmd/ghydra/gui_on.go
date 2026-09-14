//go:build gui

package main

import "github.com/xueweijian/ghydra/internal/guiapp"

// guiMain —— gui tag 构建：单二进制直通 wails 壳（W4p2 设计 D1）。
// guiapp.Run 内部 log.Fatal 会自行以非零退出；正常路径返回 0。
func guiMain() int {
	guiapp.Run()
	return 0
}
