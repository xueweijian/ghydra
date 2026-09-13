// GHydra GUI 壳（M3-W0 spike）—— Wails v3（beta.20 锁定）+ SolidJS 薄客户端。
//
// 架构铁律（PRD/TechReference M10）：GUI 不含任何加速逻辑，只是
// 常驻 daemon（ghydra serve）的控制器/观察窗，经 127.0.0.1 HTTP
// 通信。壳与浏览器（syncthing 兜底模式）共用同一份前端 dist。
//
// 本 spike 验证三件套的 v3 原生实现：
//   - 托盘常驻：SystemTray（进程内，AttachWindow 点击唤起/隐藏窗口）
//   - 开机自启：AutostartManager（win Run 键 / macOS LaunchAgent / XDG autostart）
//   - 单实例唤醒：SingleInstance（二次启动 → 首实例回调 → 显示窗口）
package main

import (
	"embed"
	"io/fs"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed assets/icon.png
var iconPNG []byte

//go:embed all:frontend/dist
var distFS embed.FS

func main() {
	distSub, err := fs.Sub(distFS, "frontend/dist")
	if err != nil {
		log.Fatalf("前端资源挂载失败: %v", err)
	}

	var app *application.App
	app = application.New(application.Options{
		Name:        "GHydra",
		Description: "GitHub 全链路加速器",
		Icon:        iconPNG,
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(distSub),
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "cn.1ciyuan.ghydra.gui",
			OnSecondInstanceLaunch: func(data application.SecondInstanceData) {
				// 二次启动 → 已有实例把面板带到前台
				if w, ok := app.Window.GetByName("main"); ok {
					w.Show()
					w.UnMinimise()
					w.Focus()
				}
			},
		},
	})

	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:            "main",
		Title:           "GHydra",
		Width:           980,
		Height:          680,
		MinWidth:        720,
		MinHeight:       480,
		HideOnFocusLost: false,
	})
	// 关窗 = 隐藏（托盘常驻）；真正退出走托盘菜单
	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		win.Hide()
		e.Cancel()
	})

	tray := app.SystemTray.New()
	tray.SetIcon(iconPNG)
	tray.SetTooltip("GHydra — GitHub 加速器")

	trayMenu := app.NewMenu()
	trayMenu.Add("打开面板").OnClick(func(*application.Context) {
		win.Show()
		win.UnMinimise()
		win.Focus()
	})
	trayMenu.Add("开机自启：切换").OnClick(func(*application.Context) {
		enabled, err := app.Autostart.IsEnabled()
		if err != nil {
			log.Printf("[autostart] 查询失败: %v", err)
			return
		}
		if enabled {
			err = app.Autostart.Disable()
		} else {
			err = app.Autostart.Enable()
		}
		if err != nil {
			log.Printf("[autostart] 切换失败: %v", err)
			return
		}
		log.Printf("[autostart] 已切换为 %t", !enabled)
	})
	trayMenu.Add("退出").OnClick(func(*application.Context) { app.Quit() })
	tray.SetMenu(trayMenu)

	// 托盘点击 = 窗口唤起/隐藏（v3 原生 attach 行为）
	tray.AttachWindow(win).WindowOffset(8)

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
