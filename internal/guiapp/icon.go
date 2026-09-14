package guiapp

import _ "embed"

// IconPNG 应用图标 —— 单一事实源（W4p2 P1 并轨时建立）：壳窗口/托盘/
// spikecheck 共用同一份。无 build tag：spikecheck 不带 gui tag 也引用它；
// gui.go（wails 壳本体）带 //go:build gui。
//
//go:embed assets/icon.png
var IconPNG []byte
