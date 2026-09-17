//go:build gui

package main

import "github.com/xueweijian/ghydra/internal/guiapp"

// guiMain —— gui tag 构建：单二进制直通 wails 壳（W4p2 设计 D1）。
// guiapp.Run 内部 log.Fatal 会自行以非零退出；正常路径返回 0。
func guiMain() int {
	guiapp.Run()
	return 0
}

// noArgsDispatch —— 无参数直达面板（v1.0.3 PR1）：双击 exe / 无参快捷
// 方式不再 usage+exit 2 闪退。单实例机制兜底双开；CLI 子命令语义不变。
// 一行壳层直通，行为由 ci-gui 构建冒烟 + 真机走查覆盖（起真窗口不可单测）。
func noArgsDispatch() int {
	return guiMain()
}
