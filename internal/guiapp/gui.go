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
	"time"

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

// connReloader webview 连接收口（F13 终版）：token/端口就绪或变化时，
// SetURL 携 ?token= 重载面板——index.tsx 落地 token 进 localStorage，
// daemonBase 由 client.ts 按 wails 虚拟域判定。全程 Go 侧原生调用，
// 不依赖 wails runtime/ExecJS/WindowRuntimeReady（beta.20 该链不稳：
// rc1 起 [inject] 零出现，dev/prod runtime 实测 _wails 安装时有时无，
// 且 WebView2 origin=http://wails.localhost 使 client.ts 的同源判定
// 误入浏览器兜底分支——三层叠加，故弃 ExecJS 改 SetURL）。
type connReloader struct {
	mu   sync.Mutex
	last string // 上次应用的 token@port 键（幂等去重）
	pump bool   // 主循环泵送门：SetURL 内部 InvokeSync，过早调用（app.Run
	// 主循环未起）实测崩进程（Chrome_WidgetWin 注销错误）——用时间门
	// 而非 wails 事件（WindowRuntimeReady 在 beta.20 不可靠，F13 实证）。
}

// markPump 由 Run 尾部的定时器置位（app.Run 已泵送）。
func (c *connReloader) markPump() {
	c.mu.Lock()
	c.pump = true
	c.mu.Unlock()
}

// apply 幂等：泵送就绪且键变化才重载（SSE 重载后原生重连，正常 tick 零开销）。
func (c *connReloader) apply() {
	c.mu.Lock()
	pump, same := c.pump, false
	c.mu.Unlock()
	if !pump {
		return
	}
	dir := supervisor.DefaultDir()
	tok := supervisor.ReadToken(dir)
	port := supervisor.ReadServePort(dir)
	if tok == "" || port == 0 {
		return // token 未生成 / daemon 未起
	}
	key := fmt.Sprintf("%s@%d", tok, port)
	c.mu.Lock()
	same = key == c.last
	c.mu.Unlock()
	if same {
		return
	}
	win.SetURL(fmt.Sprintf("/?token=%s", tok))
	c.mu.Lock()
	c.last = key
	c.mu.Unlock()
	log.Printf("[inject] SetURL 已应用连接（port=%d）", port)
}

// win 在 Run 里赋值（app.Window 创建后）。
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

	inj := &connReloader{}
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

	tray := app.SystemTray.New()
	tray.SetIcon(IconPNG)
	tray.SetTooltip("GHydra — GitHub 加速器")

	// P4c：监督循环（启动保活 + 更新继任 + tooltip + 连接注入）。
	// OnState 来自监督 goroutine——v3 公开 setter 内部主线程封送；
	// 真机走查验证项（P5 清单）。
	lastPhase := "" // 相位沿（stopped 每 2s 重发——终态唤起只做一次）
	sup := supervisor.New(supervisor.Config{
		ExePath: exePath,
		OnState: func(s supervisor.State) {
			tray.SetTooltip(tooltipFor(s))
			if s.Phase == "running" || s.Phase == "updated" {
				inj.apply()
			}
			// F12：daemon 死亡（CLI off / 崩溃）壳不做僵尸——面板带到
			// 前台明示终态；托盘菜单「重启 daemon」显式拉起（不破坏
			// off 保护：无显式意图的死亡绝不复活）。
			if s.Phase != lastPhase {
				if s.Phase == "stopped" || s.Phase == "failed" {
					win.Show()
					win.UnMinimise()
					win.Focus()
				}
				lastPhase = s.Phase
			}
		},
		Logf: log.Printf,
	})
	go sup.Run(context.Background())
	// 3s 泵送门：app.Run 主循环必然已泵送，SetURL 安全（见 connReloader.pump）
	time.AfterFunc(3*time.Second, inj.markPump)

	trayMenu := app.NewMenu()
	trayMenu.Add("打开面板").OnClick(func(*application.Context) {
		win.Show()
		win.UnMinimise()
		win.Focus()
	})
	trayMenu.Add("重启 daemon").OnClick(func(*application.Context) {
		log.Printf("[tray] 用户请求重启 daemon（显式意图，一次性覆盖 off 保护）")
		sup.Restart()
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

	// v1.0.3 PR4：弃用 AttachWindow 默认 toggle（再点一次=隐藏——用户
	// 「面板莫名消失/唤不回」的来源），改显式 OnClick：点击恒为唤起/置前，
	// 隐藏只走关窗 X（Hide）与「隐藏面板」语义，行为可预期。
	tray.OnClick(func() {
		if win.IsVisible() {
			win.Focus()
			return
		}
		win.Show()
		win.UnMinimise()
		win.Focus()
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
