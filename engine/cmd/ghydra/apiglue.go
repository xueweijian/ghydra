package main

// apiglue.go —— serveCmd 的 API 装配辅助（M3-W1）。
//
// 这里只放「映射/托管/工厂」类纯辅助；Deps 闭包本体在 serveCmd 里
// （需要 ln/ovMap/chRouter 等局部装配态）。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/xueweijian/ghydra/engine/api"
	"github.com/xueweijian/ghydra/engine/channel"
	"github.com/xueweijian/ghydra/engine/get"
	"github.com/xueweijian/ghydra/engine/proxy"
	"github.com/xueweijian/ghydra/engine/sched"
)

// OverrideName channel.Override 的可读名（api 契约用，避免 rune 转换坑）。
func OverrideName(o channel.Override) string {
	switch o {
	case channel.OverrideForceA:
		return "force_a"
	case channel.OverrideForceB:
		return "force_b"
	default:
		return ""
	}
}

// mapChannel channel.Snapshot → api.ChannelSnapshot（snake_case 契约）。
func mapChannel(s channel.Snapshot) api.ChannelSnapshot {
	out := api.ChannelSnapshot{
		State:       s.State.String(),
		Override:    OverrideName(s.Override),
		FailRate:    s.FailRate,
		Samples:     s.Samples,
		TripWhy:     s.TripWhy,
		BackoffMult: s.BackoffMult,
		BOK:         s.BOK,
		BSuppressed: s.BSuppressed,
		BSuppWhy:    s.BSuppWhy,
	}
	if !s.TripAt.IsZero() {
		t := s.TripAt
		out.TripAt = &t
	}
	if !s.OpenUntil.IsZero() {
		t := s.OpenUntil
		out.OpenUntil = &t
	}
	if !s.BAt.IsZero() {
		t := s.BAt
		out.BAt = &t
	}
	return out
}

// mapIPs sched.IPInfo → api.IPEntry。
func mapIPs(infos []sched.IPInfo) []api.IPEntry {
	out := make([]api.IPEntry, 0, len(infos))
	for _, i := range infos {
		out = append(out, api.IPEntry{
			Addr: i.Addr, State: i.State.String(), Score: i.Score, RTTMS: i.RTTMS,
			FailRate: i.FailRate, Samples: i.Samples, CooldownCount: i.CooldownCount,
		})
	}
	return out
}

// mapConn proxy.Event → api.ConnEvent。
func mapConn(e proxy.Event) api.ConnEvent {
	ok := e.DialErr == nil && !sched.HandshakeDead(e)
	errMsg := ""
	if e.DialErr != nil {
		errMsg = e.DialErr.Error()
	} else if e.CopyErr != "" {
		errMsg = "copy:" + e.CopyErr
	}
	return api.ConnEvent{
		Host: e.Host, Target: e.Target, Accel: e.Accel, OK: ok,
		DialMS: e.DialMS, Rx: e.Rx, Tx: e.Tx, DurMS: e.Duration,
		Err: errMsg, TS: e.StartedAt,
	}
}

// staticSPA 静态面板托管（SPA fallback → index.html）。dir 为空或不存在
// 返回 nil（CLI-only 安装不启面板，行为零变化）。
func staticSPA(dir string) http.Handler {
	if dir == "" {
		return nil
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil
	}
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// F13：/wails/ 前缀显式 404——浏览器面板模式不提供 wails runtime
		//（壳专属，index.html 引用脚本在此场景静默失败）。若不拦，SPA
		// fallback 会把 /wails/runtime.js 回成 index.html，前端把它当 JS
		// 解析触发语法错误。
		if strings.HasPrefix(r.URL.Path, "/wails/") {
			http.NotFound(w, r)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if f, err := os.Open(dir + string(os.PathSeparator) + p); err == nil {
			f.Close()
			fs.ServeHTTP(w, r)
			return
		}
		// SPA fallback：非资产路径一律回 index.html（前端路由接管）
		http.ServeFile(w, r, dir+string(os.PathSeparator)+"index.html")
	})
}

// decodeJSONBody 宽松解析（api 包已做严格校验，这里容忍缺省字段）。
func decodeJSONBody(b []byte, v any) {
	if len(b) > 0 {
		_ = json.Unmarshal(b, v)
	}
}

// guiDirOrDefault 面板目录解析：显式 flag 优先，否则 exe 旁 dist/。
func guiDirOrDefault(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "dist")
}

// getDownloaderFactory 下载器工厂（任务注册表注入）：A=IP 择优，
// B=CDN 前缀（运行时读 cdnFn，支持热更）。cdnFn 空 = A-only。
func getDownloaderFactory(pick func(string) (string, bool), cdnFn func() string) func() *get.Downloader {
	return func() *get.Downloader {
		var b get.Fetcher
		if p := cdnFn(); p != "" {
			b = get.NewBFetcher(p)
		}
		return get.New(get.DefaultConfig(), get.NewAFetcher(pick), b)
	}
}
