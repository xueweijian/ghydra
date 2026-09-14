//go:build gui

// Package guiapp —— GHydra GUI 壳（M3-W0 spike 转正，W4p2 P1 并轨主 module）。
// Wails v3（beta.20 锁定）+ SolidJS 薄客户端。
//
// 架构铁律（PRD/TechReference M10）：GUI 不含任何加速逻辑，只是
// 常驻 daemon（ghydra serve）的控制器/观察窗，经 127.0.0.1 HTTP
// 通信。壳与浏览器（syncthing 兜底模式）共用同一份前端 dist。
//
// 编译边界（W4p2 设计 D1）：本文件仅 -tags gui 时编译——默认构建
// 零 wails 依赖；无 tag 构建下 `ghydra gui` 走 gui_off.go 降级引导。
// Windows 构建 CGO_ENABLED=0（go-webview2 纯 Go）；Linux v1.0 CLI-only
// （GTK 需 CGO，gui 变体不进 Linux 发布矩阵）。
//
// 已验证的 v3 原生三件套：
//   - 托盘常驻：SystemTray（进程内，AttachWindow 点击唤起/隐藏窗口）
//   - 开机自启：AutostartManager（win Run 键 / macOS LaunchAgent / XDG autostart）
//   - 单实例唤醒：SingleInstance（二次启动 → 首实例回调 → 显示窗口）
//
// P4c supervisor（D1 hybrid 步骤 2 + 两段式第二段）：壳启动探活 daemon
// 未运行则拉起托管 serve；watch 更新继任（daemon 死 + update.json pending
// → spawn 继任）；阶段回调驱动托盘 tooltip（D8）+ webview 连接注入。
package guiapp

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"os"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/xueweijian/ghydra/internal/supervisor"
)

//go:embed all:frontend/dist
var distFS embed.FS

// tooltipFor 阶段 → 托盘 tooltip（D8）。
func tooltipFor(s supervisor.State) string {
	switch s.Phase {
	case "running":
		return "GHydra v" + s.Version + " — GitHub 加速器"
	case "pending":
		return "GHydra — 更新进行中（→ v" + s.Pending + "）"
	case "restarting":
		return "GHydra — 更新已安装，重启生效中…"
	case "updated":
		return "GHydra v" + s.Version + " — 已更新"
	case "stopped":
		return "GHydra — daemon 未运行（加速关）"
	case "failed":
		return "GHydra — daemon 重启失败（查看日志）"
	default:
		return "GHydra — GitHub 加速器"
	}
}

// connInjector webview 连接注入（壳模式 UX 收口）：把 api-token 与实际
// 端口写进 localStorage（client.ts 每请求现读，无需重载即生效；端口
// 变化时 location.reload 重挂 SSE）。首次注入发生在页面加载后——
// WindowRuntimeReady 前到达的状态会挂起，ready 后补注。
type connInjector struct {
	mu      sync.Mutex
	ready   bool
	pending bool
	lastIns int // 上次注入的端口（0 = 未注入过）
}

func (c *connInjector) onReady() {
	c.mu.Lock()
	c.ready = true
	do := c.pending
	c.pending = false
	c.mu.Unlock()
	if do {
		c.inject()
	}
}

func (c *connInjector) inject() {
	c.mu.Lock()
	if !c.ready {
		c.pending = true
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	dir := supervisor.DefaultDir()
	tok := supervisor.ReadToken(dir)
	port := supervisor.ReadServePort(dir)
	if tok == "" || port == 0 || port == c.lastIns {
		return // token 未生成 / daemon 未起 / 端口未变（更新重启同端口无需 reload，SSE 原生重连）
	}
	c.lastIns = port
	win.ExecJS(fmt.Sprintf(
		`localStorage.setItem('ghydra.token',%q);localStorage.setItem('ghydra.daemonBase','http://127.0.0.1:%d');location.reload();`,
		tok, port))
	log.Printf("[inject] 已注入 token + 127.0.0.1:%d", port)
}

// win 在 inject 闭包里赋值（app.Window 创建后）。
var win *application.WebviewWindow

// Run 启动 GUI 壳（阻塞直至退出；致命错误 log.Fatal 非零退出）。
func Run() {
	// spawn 三课 #1：exe 路径必须在入口预捕获——自更新交换后
	// os.Executable()（Linux /proc/self/exe）指向 .old，会拉起旧版。
	exePath, err := os.Executable()
	if err != nil {
		log.Fatalf("自身路径不可用: %v", err)
	}

	distSub, err := fs.Sub(distFS, "frontend/dist")
	if err != nil {
		log.Fatalf("前端资源挂载失败: %v", err)
	}

	inj := &connInjector{}
	var app *application.App
	app = application.New(application.Options{
		Name:        "GHydra",
		Description: "GitHub 全链路加速器",
		Icon:        IconPNG,
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

	win = app.Window.NewWithOptions(application.WebviewWindowOptions{
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
	win.RegisterHook(events.Common.WindowRuntimeReady, func(e *application.WindowEvent) {
		inj.onReady() // 页面就绪后才允许 ExecJS 注入
	})

	tray := app.SystemTray.New()
	tray.SetIcon(IconPNG)
	tray.SetTooltip("GHydra — GitHub 加速器")

	// P4c：监督循环（启动保活 + 更新继任 + tooltip + 连接注入）。
	// OnState 来自监督 goroutine——v3 公开 setter 内部主线程封送；
	// 真机走查验证项（P5 清单）。
	sup := supervisor.New(supervisor.Config{
		ExePath: exePath,
		OnState: func(s supervisor.State) {
			tray.SetTooltip(tooltipFor(s))
			if s.Phase == "running" || s.Phase == "updated" {
				inj.inject()
			}
		},
		Logf: log.Printf,
	})
	go sup.Run(context.Background())

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
