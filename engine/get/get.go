// Package get 实现 ghydra get 下载器（M2-W2，方案 D4）。
//
// 择路策略：默认 A（IP 择优直连）起步；TTFB > 2s，或总量 > 10MB 且
// 前 1MB 实测 < 200KB/s → 判 A 慢，Range: bytes=N- 切 B（CDN 前缀）
// 续传；B 失败/不一致 → 回 A 续传，双通道对照进报告（诊断价值）。
//
// 一致性防护（防 CDN 缓存错配）：Content-Range 总量与 A 的
// Content-Length 不一致，或双侧 ETag 都存在且不同 → 弃 B 回 A。
package get

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Fetcher 一条下载路径。from=-1 完整 GET；from≥0 发 Range: bytes=from-。
// 调用方负责关闭返回的 Body。
type Fetcher interface {
	Get(ctx context.Context, rawURL string, from int64) (*http.Response, error)
}

// Config 吞吐启发参数（零值用 DefaultConfig）。
type Config struct {
	TTFBSlow    time.Duration // TTFB 超过即判 A 慢（默认 2s）
	ProbeBytes  int64         // 前 N 字节测速窗口（默认 1MB）
	ProbeMinBPS float64       // 窗口速率低于即判 A 慢（默认 200KB/s）
	BigFile     int64         // 总量不超过此值不触发切道（默认 10MB）
	MaxSwitches int           // 最多切换次数（默认 3，防乒乓）
}

func DefaultConfig() Config {
	return Config{
		TTFBSlow:    2 * time.Second,
		ProbeBytes:  1 << 20,
		ProbeMinBPS: 200 * 1024,
		BigFile:     10 << 20,
		MaxSwitches: 3,
	}
}

// Segment 一段通道使用记录（一次下载可能多段：A→B→A）。
type Segment struct {
	Channel  string  `json:"channel"` // "A"/"B"
	StartOff int64   `json:"start_off"`
	Bytes    int64   `json:"bytes"`
	DurMS    float64 `json:"dur_ms"`
	TTFBMS   float64 `json:"ttfb_ms"`
	RateBPS  float64 `json:"rate_bps"`
	WhyOut   string  `json:"why_out,omitempty"` // 离开该通道的原因
}

// Result 一次下载的完整报告（双通道对照）。
type Result struct {
	URL        string    `json:"url"`
	TotalBytes int64     `json:"total_bytes"` // 服务端声明（-1 未知）
	GotBytes   int64     `json:"got_bytes"`   // 实际落盘
	DurMS      float64   `json:"dur_ms"`
	RateBPS    float64   `json:"rate_bps"` // 端到端
	Segments   []Segment `json:"segments"`
	ETag       string    `json:"etag,omitempty"`
	FinalCh    string    `json:"final_channel"`
}

// Downloader 择路下载器。A 必须提供；B 为 nil 时禁用切道。
type Downloader struct {
	cfg Config
	a   Fetcher
	b   Fetcher
}

// New 构造。b 可为 nil（无 B 通道，A-only 模式）。
func New(cfg Config, a, b Fetcher) *Downloader {
	if cfg.ProbeBytes <= 0 {
		cfg = DefaultConfig()
	}
	if cfg.MaxSwitches <= 0 {
		cfg.MaxSwitches = 3
	}
	return &Downloader{cfg: cfg, a: a, b: b}
}

// Get 下载 url 到 dst（已存在则覆盖）。并发安全（无共享态）。
func (d *Downloader) Get(ctx context.Context, rawURL, dst string) (Result, error) {
	start := time.Now()
	res := Result{URL: rawURL, TotalBytes: -1}
	f, err := os.Create(dst)
	if err != nil {
		return res, err
	}
	defer f.Close()

	cur := "A"
	var offset int64
	switches := 0
	first := true // 吞吐启发只跑首段：续传段重跑会造成 A 完成后仍被
	// 判 slow → 再切 B → 乒乓（Windows 定时器粒度下实测复现）

	for {
		fetch := d.a
		name := "A"
		if cur == "B" {
			fetch = d.b
			name = "B"
		}
		from := int64(-1)
		if offset > 0 {
			from = offset
		}
		// 一致性期望（首段之后的段必须对上首段的声明，防 CDN 缓存错配）
		seg, total, etag, slow, err := d.stream(ctx, fetch, rawURL, from, f, offset,
			first && cur == "A" && d.b != nil, res.TotalBytes, res.ETag)
		first = false
		seg.Channel = name
		seg.StartOff = offset
		if err != nil && seg.WhyOut == "" {
			seg.WhyOut = err.Error()
		}
		res.Segments = append(res.Segments, seg)
		if res.ETag == "" {
			res.ETag = etag
		}
		if total > 0 && res.TotalBytes <= 0 {
			res.TotalBytes = total
		}
		if err != nil && seg.WhyOut == "" {
			seg.WhyOut = err.Error()
		}
		offset += seg.Bytes
		res.GotBytes = offset

		if err != nil {
			// 通道报错：A→B / B→A 交替；无 B 或次数耗尽则返回错误
			if d.b != nil && switches < d.cfg.MaxSwitches {
				switches++
				seg.WhyOut = fmt.Sprintf("错误切道(%d): %v", switches, err)
				cur = other(cur)
				continue
			}
			seg.WhyOut = "错误: " + err.Error()
			res.DurMS = ms(time.Since(start))
			return res, fmt.Errorf("下载失败（已写 %d 字节）: %w", offset, err)
		}
		if seg.WhyOut != "" { // stream 内部已写原因
			// slow=true 且还有切换余量 → 切道续传
			if slow && d.b != nil && switches < d.cfg.MaxSwitches {
				switches++
				cur = other(cur)
				continue
			}
		}
		// 正常完成
		res.FinalCh = cur
		res.DurMS = ms(time.Since(start))
		res.RateBPS = bps(offset, res.DurMS)
		return res, nil
	}
}

// stream 从 fetch 拉流写入 f（offset 起写）。返回（段指标, 总量, etag,
// 是否判慢, 错误）。probe=true 时执行 TTFB/前 N 字节启发（仅 A 首段）。
// wantTotal/wantETag 为跨通道一致性期望（首段后传入；头部即校验，
// 不等 body 落盘）。零值表示未知、不校验。
func (d *Downloader) stream(ctx context.Context, fetch Fetcher, rawURL string, from int64,
	f *os.File, offset int64, probe bool, wantTotal int64, wantETag string) (seg Segment, total int64, etag string, slow bool, err error) {

	t0 := time.Now()
	var resp *http.Response
	if from >= 0 {
		resp, err = fetch.Get(ctx, rawURL, from)
	} else {
		resp, err = fetch.Get(ctx, rawURL, -1)
	}
	if err != nil {
		seg.TTFBMS = ms(time.Since(t0))
		return seg, -1, "", false, err
	}
	defer resp.Body.Close()
	seg.TTFBMS = ms(time.Since(t0))

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return seg, -1, "", false, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusPartialContent {
		if t, ok := parseContentRangeTotal(resp.Header.Get("Content-Range")); ok {
			total = t
		}
	} else if resp.ContentLength > 0 {
		total = resp.ContentLength
	}
	etag = resp.Header.Get("ETag")

	// 跨通道一致性（防 CDN 缓存错配）：头部阶段拦截，不污染落盘数据
	if wantTotal > 0 && total > 0 && total != wantTotal {
		return seg, total, etag, false,
			fmt.Errorf("总量 %d ≠ 首段 %d（疑似缓存错配）", total, wantTotal)
	}
	if wantETag != "" && etag != "" && etag != wantETag {
		return seg, total, etag, false, fmt.Errorf("ETag 与首段不一致")
	}

	// TTFB 启发：头部到得太慢且是大文件 → 不等 body，直接判慢切道
	if probe && total > d.cfg.BigFile && seg.TTFBMS > float64(d.cfg.TTFBSlow/time.Millisecond) {
		seg.WhyOut = fmt.Sprintf("TTFB %.0fms 超过 %v", seg.TTFBMS, d.cfg.TTFBSlow)
		return seg, total, etag, true, nil
	}

	buf := make([]byte, 64<<10)
	probeBudget := d.cfg.ProbeBytes
	probeStart := time.Now()
	var probeBytes int64

	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.WriteAt(buf[:n], offset); werr != nil {
				return seg, total, etag, false, werr
			}
			offset += int64(n)
			seg.Bytes += int64(n)

			if probe && probeBudget > 0 {
				probeBytes += int64(n)
				probeBudget -= int64(n)
				if probeBytes >= d.cfg.ProbeBytes {
					// 前 N 字节窗口闭合：判慢即中断当前流（切道续传）
					elapsed := ms(time.Since(probeStart))
					if elapsed < 1 {
						elapsed = 1 // Windows 定时器粒度下快速窗口可能测得 ~0ms
					}
					rate := bps(probeBytes, elapsed)
					seg.RateBPS = rate
					if total > d.cfg.BigFile && rate < d.cfg.ProbeMinBPS {
						seg.WhyOut = fmt.Sprintf("前%dKB=%.0fKB/s 低于阈值", d.cfg.ProbeBytes>>10, rate/1024)
						return seg, total, etag, true, nil
					}
					probe = false // 窗口已测：不慢则继续流完
				}
			}
		}
		if rerr == io.EOF {
			seg.DurMS = ms(time.Since(t0))
			seg.RateBPS = bps(seg.Bytes, seg.DurMS)
			return seg, total, etag, false, nil
		}
		if rerr != nil {
			return seg, total, etag, false, rerr
		}
		if ctx.Err() != nil {
			return seg, total, etag, false, ctx.Err()
		}
	}
}

func other(ch string) string {
	if ch == "A" {
		return "B"
	}
	return "A"
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func bps(bytes int64, durMS float64) float64 {
	if durMS <= 0 {
		return 0
	}
	return float64(bytes) / (durMS / 1000)
}

// parseContentRangeTotal 解析 "bytes 100-999/5000" 的 5000。
func parseContentRangeTotal(cr string) (int64, bool) {
	i := strings.IndexByte(cr, '/')
	if i < 0 || i+1 >= len(cr) {
		return 0, false
	}
	tail := strings.TrimSpace(cr[i+1:])
	if tail == "*" {
		return 0, false
	}
	var n int64
	if _, err := fmt.Sscanf(tail, "%d", &n); err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// ErrNoB 无 B 通道可用。
var ErrNoB = errors.New("未配置 B 通道")
