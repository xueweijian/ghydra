package api

// dto.go —— /api/* 的响应 DTO（M3-W1）。
//
// 设计：api 包不 import channel/probe/get/store，契约类型全部在本包
// 定义（json tag + golden 锁定），cmd 装配时做字段映射。TS 侧 types.ts
// 与 testdata/golden/*.json 一一对应（vitest 编译期锁定）。

import "time"

// APIVersion /api 契约版本（破坏性变更时 +1，前端按此分支）。
const APIVersion = 1

// ---- GET /api/status ----

// ApiStatus daemon 状态全景（SSE status 帧同形状）。
type ApiStatus struct {
	APIVersion int                     `json:"api_version"`
	Version    string                  `json:"version"` // 二进制版本（ldflags 注入，dev 兜底；W4 自更新地基）
	Listen     string                  `json:"listen"`
	Scheduler  bool                    `json:"scheduler"`
	Conns      int64                   `json:"conns"`
	UptimeS    float64                 `json:"uptime_s"`
	Pools      map[string]PoolSnapshot `json:"pools,omitempty"`
	Channel    ChannelSnapshot         `json:"channel"`
	CDN        string                  `json:"cdn,omitempty"`
	Rules      RulesStatus             `json:"rules"`
	Takeover   TakeoverState           `json:"takeover"`         // 系统代理接管态（W3a）
	Update     *UpdateStatus           `json:"update,omitempty"` // 自更新状态机（P4/D7；nil = 未装配/idle 不推）
}

// TakeoverState 接管态：on = 快照行存在（崩溃对账同源判定）。
// 不带 mode——快照存恢复凭证（原值），mode 不入快照（W3-Design §3.1）。
type TakeoverState struct {
	On    bool   `json:"on"`
	Since string `json:"since"` // RFC3339 UTC，on=false 时空
}

// RulesStatus status/SSE status 帧内的规则摘要（M3-W2：热更新可观测）。
type RulesStatus struct {
	Version int64  `json:"version"`
	Source  string `json:"source"` // embedded | disk | remote
	Stale   bool   `json:"stale"`  // 软过期（A6：可观测不失效）
}

// RulesSnapshot GET /api/rules 响应：当前信任地板快照全景。
type RulesSnapshot struct {
	Version      int64               `json:"version"`
	Source       string              `json:"source"`
	Stale        bool                `json:"stale"`
	GeneratedAt  string              `json:"generated_at"` // RFC3339 UTC
	ExpiresAt    string              `json:"expires_at"`
	Domains      []string            `json:"domains"`
	CDNEndpoints []string            `json:"cdn_endpoints"`
	SeedIPs      map[string][]string `json:"seed_ips"`
	Refresh      RulesRefreshState   `json:"refresh"`
}

// RulesRefreshState 拉取管线状态（W2 phase 3 refresher 填实；schema 先行
// 锁定，避免 golden 二次迁移）。LastResult: ""=从未拉取 / "ok" /
// "reject:回滚|快进|验签|schema" / "error:网络…"。
type RulesRefreshState struct {
	LastResult string `json:"last_result"`
	LastAt     string `json:"last_at"` // RFC3339 UTC，空 = 从未
	NextAt     string `json:"next_at"` // 下次自动拉取时刻，空 = 无计划
	Running    bool   `json:"running"`
}

// PoolSnapshot 单域名 IP 池快照。
type PoolSnapshot struct {
	Sticky string    `json:"sticky"` // 粘性 IP（空 = 无）
	IPs    []IPEntry `json:"ips"`
	// F3 冷/热延迟度量（数据出真理；0 = 尚无成功样本）。
	ColdFirstMS float64 `json:"cold_first_ms,omitempty"` // 域池建立 → 首个成功样本
	WarmP50MS   float64 `json:"warm_p50_ms,omitempty"`   // 最近成功拨号耗时中位数
}

// IPEntry 池内单 IP 状态（sched.IPInfo 映射）。
type IPEntry struct {
	Addr          string  `json:"addr"`
	State         string  `json:"state"`
	Score         float64 `json:"score"`
	RTTMS         float64 `json:"rtt_ms"`
	FailRate      float64 `json:"fail_rate"`
	Samples       int     `json:"samples"`
	CooldownCount int     `json:"cooldown_count"`
}

// ChannelSnapshot 通道决策器快照（channel.Snapshot 映射，snake_case）。
type ChannelSnapshot struct {
	State       string     `json:"state"`
	Override    string     `json:"override,omitempty"`
	FailRate    float64    `json:"fail_rate"`
	Samples     int        `json:"samples"`
	TripAt      *time.Time `json:"trip_at,omitempty"`
	TripWhy     string     `json:"trip_why,omitempty"`
	OpenUntil   *time.Time `json:"open_until,omitempty"`
	BackoffMult int        `json:"backoff_mult"`
	BOK         *bool      `json:"b_ok,omitempty"` // nil = 未知（未探测）
	BAt         *time.Time `json:"b_at,omitempty"`
	BSuppressed int        `json:"b_suppressed"`
	BSuppWhy    string     `json:"b_supp_why,omitempty"`
}

// ---- GET/POST /api/config ----

// ApiConfig 运行配置快照。CDN 标注可 POST 热更；其余字段写入持久化
// config 表（P4/D6），重启 daemon 生效（响应 requires_restart 列出）。
type ApiConfig struct {
	Listen         string `json:"listen"`
	Scheduler      bool   `json:"scheduler"`
	CDN            string `json:"cdn"` // hot：POST /api/config 热更
	DoctorEveryS   int64  `json:"doctor_every_s"`
	DoctorRepo     string `json:"doctor_repo"`
	Managed        bool   `json:"managed"`
	GUIDist        string `json:"gui_dist,omitempty"`
	RulesURL       string `json:"rules_url"`        // P4/D6 持久化（重启生效）
	RulesIntervalS int64  `json:"rules_interval_s"` // P4/D6 持久化（重启生效）
}

// ConfigSetResp POST /api/config 响应：新配置快照 + 需重启生效的字段集
// （requires_restart 空 = 全部已热更）。
type ConfigSetResp struct {
	ApiConfig
	RequiresRestart []string `json:"requires_restart,omitempty"`
}

// ConfigPatch POST /api/config 请求体。指针区分「未提供」与「置空」
// （cdn 置空 = 关 B 通道切道）。全部 nil = 400 无可写字段。
type ConfigPatch struct {
	CDN            *string `json:"cdn"`
	RulesURL       *string `json:"rules_url"`        // P4/D6：持久化，重启生效
	RulesIntervalS *int64  `json:"rules_interval_s"` // P4/D6：秒；持久化，重启生效
	Listen         *string `json:"listen"`           // P4/D6：127.0.0.1:port；持久化，重启生效
	DoctorEveryS   *int64  `json:"doctor_every_s"`   // P4/D6：秒（0=关）；持久化，重启生效
}

// ---- /api/update/*（P4/D7）----

// UpdateInfo GET /api/update/check 响应（同步；装配层 5min 缓存）。
type UpdateInfo struct {
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"`
	HasUpdate bool   `json:"has_update"`
	Notes     string `json:"notes,omitempty"` // changelog 首行
	HTMLURL   string `json:"html_url,omitempty"`
	CheckedAt string `json:"checked_at"` // RFC3339 UTC
	Cached    bool   `json:"cached"`     // 缓存命中
	Error     string `json:"error,omitempty"`
}

// UpdateStatus 状态机快照（GET /api/update/status + SSE status 帧 update 子对象）。
// state: idle | checking | downloading | verifying | swapping | pending_boot | failed
type UpdateStatus struct {
	State     string  `json:"state"`
	Current   string  `json:"current"`
	Target    string  `json:"target,omitempty"`
	Progress  float64 `json:"progress_pct,omitempty"` // 0-100（downloading 态）
	Error     string  `json:"error,omitempty"`
	UpdatedAt string  `json:"updated_at"` // RFC3339 UTC
}

// UpdateApplyResp POST /api/update/apply 响应（异步已启动）。
type UpdateApplyResp struct {
	Started bool   `json:"started"`
	ApplyID string `json:"apply_id"`
}

// ---- POST /api/on | /api/off ----

// OnReq POST /api/on 请求体（mode 空 = pac）。
type OnReq struct {
	Mode string `json:"mode"` // "pac"（默认）| "proxy"
}

// OnResp POST /api/on 响应。
type OnResp struct {
	OK   bool   `json:"ok"`
	Mode string `json:"mode"`
}

// OffReq POST /api/off 请求体。
type OffReq struct {
	Shutdown bool `json:"shutdown"` // true = 恢复后停 daemon
}

// OffResp POST /api/off 响应。
type OffResp struct {
	OK       bool `json:"ok"`
	Shutdown bool `json:"shutdown"`
}

// ---- doctor ----

// DoctorRunResp POST /api/doctor/run 响应。
type DoctorRunResp struct {
	RunID   string `json:"run_id"`
	Started bool   `json:"started"`
}

// DoctorSummaryRow GET /api/doctor/summary 行（store.DoctorSummary 映射）。
type DoctorSummaryRow struct {
	Mode          string    `json:"mode"`
	Scenario      string    `json:"scenario"`
	Checks        int       `json:"checks"`
	Passed        int       `json:"passed"`
	Reachable     int       `json:"reachable"`
	AvgDurationMS float64   `json:"avg_duration_ms"`
	AvgTTFBMS     float64   `json:"avg_ttfb_ms"`
	FirstAt       time.Time `json:"first_at"`
	LastAt        time.Time `json:"last_at"`
}

// DoctorSummaryResp GET /api/doctor/summary 响应。
type DoctorSummaryResp struct {
	Runs []DoctorSummaryRow `json:"runs"`
}

// DoctorFrame SSE doctor 帧：一轮 doctor 完成（手动或周期）。
type DoctorFrame struct {
	RunID      string    `json:"run_id"`
	Verdict    string    `json:"verdict"`     // ok|unclear|network_fault|source_fault|github_down
	ProxyOK    int       `json:"proxy_ok"`    // 通道列通过数
	ProxyTotal int       `json:"proxy_total"` // 五场景
	DirectOK   int       `json:"direct_ok"`
	CDNOK      *int      `json:"cdn_ok,omitempty"` // nil = 无 B 列
	Error      string    `json:"error,omitempty"`
	FinishedAt time.Time `json:"finished_at"`
}

// ---- get 任务 ----

// GetStartReq POST /api/get/start 请求体。
type GetStartReq struct {
	URL string `json:"url"`
	Dst string `json:"dst"` // 空 = ~/Downloads/<basename>
}

// GetStartResp POST /api/get/start 响应。
type GetStartResp struct {
	TaskID string `json:"task_id"`
	Dst    string `json:"dst"`
}

// GetTask 下载任务快照（GET /api/get/progress 行 + SSE get 帧同形状）。
type GetTask struct {
	TaskID    string     `json:"task_id"`
	URL       string     `json:"url"`
	Dst       string     `json:"dst"`
	GotBytes  int64      `json:"got_bytes"`
	Total     int64      `json:"total_bytes"` // -1 = 未知
	RateBPS   float64    `json:"rate_bps"`
	Channel   string     `json:"channel"` // A/B/""（未开始）
	Done      bool       `json:"done"`
	Error     string     `json:"error,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// GetProgressResp GET /api/get/progress 响应。
type GetProgressResp struct {
	Active *GetTask  `json:"active"` // null = 无在途任务
	Recent []GetTask `json:"recent"` // 最近完成（新→旧，上限 32）
}

// ---- MITM（W2/W3 前的桩，schema 先行）----

// MitmStatus GET /api/mitm/status 响应。
type MitmStatus struct {
	Available bool   `json:"available"` // false = 引擎未上线（W2）
	Enabled   bool   `json:"enabled"`
	Reason    string `json:"reason,omitempty"`
}

// ---- SSE 帧 ----

// HelloFrame SSE hello 帧（连接确认）。
type HelloFrame struct {
	Proto      int `json:"proto"`
	HeartbeatS int `json:"heartbeat_s"`
	APIVersion int `json:"api_version"`
}

// ConnEvent 单条连接事件（proxy.Event 精简）。
type ConnEvent struct {
	Host   string    `json:"host"`
	Target string    `json:"target"`
	Accel  bool      `json:"accel"`
	OK     bool      `json:"ok"`
	DialMS float64   `json:"dial_ms"`
	Rx     int64     `json:"rx"`
	Tx     int64     `json:"tx"`
	DurMS  float64   `json:"dur_ms"`
	Err    string    `json:"err,omitempty"`
	TS     time.Time `json:"ts"`
}

// ConnFrame SSE conn 帧（≥250ms 合批）。
type ConnFrame struct {
	Events []ConnEvent `json:"events"`
}
