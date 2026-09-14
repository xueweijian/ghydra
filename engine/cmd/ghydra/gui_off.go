//go:build !gui

package main

import (
	"fmt"
	"io"
	"os"
)

// guiMain —— 无 gui tag 构建的降级路径（W4p2 设计 D1：默认构建零 wails 依赖，
// 二进制体积与依赖白名单不因 GUI 膨胀）。返回退出码而非 os.Exit（可测）。
func guiMain() int {
	return guiMainTo(os.Stderr)
}

func guiMainTo(w io.Writer) int {
	fmt.Fprint(w, `ghydra gui：此二进制未包含桌面面板。
Windows/macOS 官方发布版已内置；源码构建加 gui 标签：
  go build -tags gui -o ghydra ./engine/cmd/ghydra
`)
	return 2
}
