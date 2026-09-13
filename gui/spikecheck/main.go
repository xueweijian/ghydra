// spikecheck —— M3-W0 的 CI 可执行验证（linux + xvfb 下运行）。
//
// 验证项（三件套中 CI 可机检的部分）：
//  1. wails v3 应用栈能真实启动（gtk/webkit 运行时完整）
//  2. WebviewWindow 可创建
//  3. SystemTray 可创建并设置图标/菜单（无托盘宿主时不保证可见，只验证不崩）
//  4. Autostart 往返：Enable → IsEnabled==true → Disable → IsEnabled==false
//  5. 定时退出，进程 exit 0
//
// 单实例与窗口可见性行为属交互语义，留在真机手动清单。
package main

import (
	_ "embed"
	"fmt"
	"os"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed assets/icon.png
var iconPNG []byte

func main() {
	app := application.New(application.Options{
		Name:        "ghydra-spikecheck",
		Description: "CI smoke for wails v3 stack",
		Icon:        iconPNG,
		Assets:      application.AlphaAssets,
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:   "smoke",
		Title:  "spikecheck",
		Width:  120,
		Height: 80,
		Hidden: true,
	})

	tray := app.SystemTray.New()
	tray.SetIcon(iconPNG)
	tray.SetTooltip("ghydra spikecheck")

	trayMenu := app.NewMenu()
	trayMenu.Add("noop").OnClick(func(*application.Context) {})
	tray.SetMenu(trayMenu)

	// autostart 往返在 app.Run 的事件循环内做（平台注册需主循环就绪）
	results := make(chan error, 1)
	go func() {
		time.Sleep(2 * time.Second) // 等平台初始化
		results <- autostartRoundTrip(app)
		time.Sleep(300 * time.Millisecond)
		app.Quit()
	}()

	time.AfterFunc(60*time.Second, func() {
		fmt.Println("FAIL: 60s 超时未完成自检")
		app.Quit()
		os.Exit(3)
	})

	if err := app.Run(); err != nil {
		fmt.Printf("FAIL: app.Run: %v\n", err)
		os.Exit(2)
	}
	if err := <-results; err != nil {
		fmt.Printf("FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("PASS: wails v3 栈启动/窗口/托盘/autostart 往返 全部通过")
}

func autostartRoundTrip(app *application.App) error {
	if err := app.Autostart.Enable(); err != nil {
		return fmt.Errorf("autostart Enable: %w", err)
	}
	enabled, err := app.Autostart.IsEnabled()
	if err != nil {
		return fmt.Errorf("autostart IsEnabled: %w", err)
	}
	if !enabled {
		return fmt.Errorf("Enable 后 IsEnabled=%t", enabled)
	}
	if err := app.Autostart.Disable(); err != nil {
		return fmt.Errorf("autostart Disable: %w", err)
	}
	if enabled, err = app.Autostart.IsEnabled(); err != nil || enabled {
		return fmt.Errorf("Disable 后 IsEnabled=%t err=%v", enabled, err)
	}
	return nil
}
