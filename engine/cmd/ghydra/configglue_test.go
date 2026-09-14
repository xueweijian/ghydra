package main

// configglue_test.go —— P4/D6 L1：serve 启动 flags 合并优先级矩阵。
// 优先级（设计冻结）：显式 CLI flag（flag.Visit） > 持久化 config 表 > 默认。
// 防御面：持久化值损坏（DB 手改）时保守忽略回默认，不 fail 启动。

import (
	"flag"
	"path/filepath"
	"testing"
	"time"

	"github.com/xueweijian/ghydra/engine/store"
)

type flagCase struct {
	name      string // 用例名
	explicit  string // flag.Visit 里的 flag 名（"" = 未显式）
	flagVal   string // 显式值
	persisted string // config 表值（"" = 无持久化）
	want      string // 期望生效值
}

// runMerge 构造真实 flag.FlagSet（与 serveCmd 同名同默认）跑 applyPersistedFlags。
func runMerge(t *testing.T, c flagCase) (got map[string]string) {
	t.Helper()
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:9801", "")
	cdn := fs.String("cdn", "", "")
	rulesURL := fs.String("rules-url", "", "")
	rulesInterval := fs.Duration("rules-interval", 6*time.Hour, "")
	doctorInterval := fs.Duration("doctor-interval", 0, "")

	if c.explicit != "" {
		if err := fs.Set(c.explicit, c.flagVal); err != nil {
			t.Fatalf("Set %s=%s: %v", c.explicit, c.flagVal, err)
		}
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	persisted := map[string]string{}
	if c.persisted != "" {
		// 约定 "key=value" 单对；测试便利形态
		for i := 0; i < len(c.persisted); i++ {
			if c.persisted[i] == '=' {
				persisted[c.persisted[:i]] = c.persisted[i+1:]
				break
			}
		}
	}

	applyPersistedFlags(explicit, persisted,
		listen, cdn, rulesURL, rulesInterval, doctorInterval,
		true /*managed*/, t.Logf)

	return map[string]string{
		"listen":          *listen,
		"cdn":             *cdn,
		"rules_url":       *rulesURL,
		"rules_interval":  rulesInterval.String(),
		"doctor_interval": doctorInterval.String(),
	}
}

func TestMergePriorityMatrix(t *testing.T) {
	const (
		defListen = "127.0.0.1:9801"
		perListen = "127.0.0.1:9900"
		defCDN    = ""
		perCDN    = "https://gh-proxy.com/"
		defRURL   = ""
		perRURL   = "https://mirror.example/rules.json"
		defRInt   = "6h0m0s"
		perRInt   = "1h0m0s"
		perDocInt = "30m0s"
		mngDocInt = "1h0m0s" // managed 无持久化默认（原 spawnServe 硬编码迁移）
	)

	cases := []struct {
		key  string
		rows []flagCase
	}{
		{key: "listen", rows: []flagCase{
			{name: "显式赢持久化", explicit: "listen", flagVal: "127.0.0.1:7777", persisted: "listen=" + perListen, want: "127.0.0.1:7777"},
			{name: "持久化赢默认", persisted: "listen=" + perListen, want: perListen},
			{name: "默认兜底", want: defListen},
		}},
		{key: "cdn", rows: []flagCase{
			{name: "显式赢持久化", explicit: "cdn", flagVal: "https://x/", persisted: "cdn=" + perCDN, want: "https://x/"},
			{name: "持久化赢默认", persisted: "cdn=" + perCDN, want: perCDN},
			{name: "默认兜底", want: defCDN},
		}},
		{key: "rules_url", rows: []flagCase{
			{name: "显式赢持久化", explicit: "rules-url", flagVal: "https://a/", persisted: "rules_url=" + perRURL, want: "https://a/"},
			{name: "持久化赢默认", persisted: "rules_url=" + perRURL, want: perRURL},
			{name: "默认兜底", want: defRURL},
		}},
		{key: "rules_interval", rows: []flagCase{
			{name: "显式赢持久化", explicit: "rules-interval", flagVal: "2h", persisted: "rules_interval=" + perRInt, want: "2h0m0s"},
			{name: "持久化赢默认", persisted: "rules_interval=" + perRInt, want: perRInt},
			{name: "默认兜底", want: defRInt},
		}},
		{key: "doctor_interval", rows: []flagCase{
			{name: "显式赢持久化", explicit: "doctor-interval", flagVal: "2h", persisted: "doctor_interval=" + perDocInt, want: "2h0m0s"},
			{name: "持久化赢默认", persisted: "doctor_interval=" + perDocInt, want: perDocInt},
			{name: "managed 默认 1h", want: mngDocInt},
		}},
	}
	for _, tc := range cases {
		for _, c := range tc.rows {
			t.Run(tc.key+"/"+c.name, func(t *testing.T) {
				got := runMerge(t, c)
				if got[tc.key] != c.want {
					t.Fatalf("%s: got %q want %q", tc.key, got[tc.key], c.want)
				}
			})
		}
	}
}

func TestMergeDoctorIntervalUnmanagedDefaultOff(t *testing.T) {
	// 非 managed 且无持久化：默认 0（前台 serve 不自动 doctor——既有行为）
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:9801", "")
	cdn := fs.String("cdn", "", "")
	rulesURL := fs.String("rules-url", "", "")
	rulesInterval := fs.Duration("rules-interval", 6*time.Hour, "")
	doctorInterval := fs.Duration("doctor-interval", 0, "")
	applyPersistedFlags(map[string]bool{}, map[string]string{},
		listen, cdn, rulesURL, rulesInterval, doctorInterval, false, t.Logf)
	if doctorInterval.String() != "0s" {
		t.Fatalf("非 managed 默认应 0s: %s", doctorInterval)
	}
}

func TestMergeDefensiveCorruptPersisted(t *testing.T) {
	// 持久化值损坏（DB 手改）→ 保守忽略回默认，不 panic 不 fail 启动
	cases := []flagCase{
		{name: "listen 非回环拒绝", persisted: "listen=0.0.0.0:1234", want: "127.0.0.1:9801"},
		{name: "listen 无端口拒绝", persisted: "listen=127.0.0.1", want: "127.0.0.1:9801"},
		{name: "cdn 非法前缀拒绝", persisted: "cdn=ftp://x/", want: ""},
		{name: "rules_interval 坏 duration", persisted: "rules_interval=abc", want: "6h0m0s"},
		{name: "doctor_interval 坏 duration", persisted: "doctor_interval=xyz", want: "1h0m0s"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key := c.persisted[:indexByte(c.persisted, '=')]
			got := runMerge(t, c)
			if got[persistedKeyToFlag(key)] != c.want {
				t.Fatalf("%s: got %q want %q", key, got[persistedKeyToFlag(key)], c.want)
			}
		})
	}
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func persistedKeyToFlag(k string) string {
	switch k {
	case "rules_url":
		return "rules_url"
	case "rules_interval":
		return "rules_interval"
	case "doctor_interval":
		return "doctor_interval"
	}
	return k
}

// TestPersistedRoundtrip L2：真 store 写入 → loadPersistedConfig →
// applyPersistedFlags 全链路（持久化重启闭环的后半段；前半段
// POST /api/config→表 由真 serve smoke 覆盖）。
func TestPersistedRoundtrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rt.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for k, v := range map[string]string{
		"listen": "127.0.0.1:9900", "cdn": "https://gh-proxy.com/",
		"rules_interval": "2h", "doctor_interval": "30m",
	} {
		if err := db.ConfigSet(k, v); err != nil {
			t.Fatalf("Set %s: %v", k, v)
		}
	}
	_ = db.Close()

	vals, db2 := loadPersistedConfig(dbPath)
	if db2 == nil {
		t.Fatal("store 应可用")
	}
	defer db2.Close()

	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:9801", "")
	cdn := fs.String("cdn", "", "")
	rulesURL := fs.String("rules-url", "", "")
	rulesInterval := fs.Duration("rules-interval", 6*time.Hour, "")
	doctorInterval := fs.Duration("doctor-interval", 0, "")
	applyPersistedFlags(map[string]bool{}, vals,
		listen, cdn, rulesURL, rulesInterval, doctorInterval, true, t.Logf)

	if *listen != "127.0.0.1:9900" || *cdn != "https://gh-proxy.com/" ||
		rulesInterval.String() != "2h0m0s" || doctorInterval.String() != "30m0s" {
		t.Fatalf("roundtrip 不符: listen=%s cdn=%s rulesInt=%s docInt=%s",
			*listen, *cdn, rulesInterval, doctorInterval)
	}
}

// ---- P4/D7：updateRunner L1（状态机转移/单飞/缓存） ----

func newTestRunner(t *testing.T) *updateRunner {
	t.Helper()
	// updater 不可用的环境（StatePath 空）也必须给 idle 兜底——503 面
	// 由 nil runner 表达（装配层），runner 本身永不 nil panic。
	r := newUpdateRunner("", "", "", nil, nil, t.Logf)
	if r != nil {
		t.Cleanup(func() {})
	}
	return r
}

func TestUpdateRunnerIdleFallback(t *testing.T) {
	r := newTestRunner(t)
	if r == nil {
		t.Skip("updater 装配不可用（无 HOME），503 面由装配层保证")
	}
	st := r.Status()
	if st.State != "idle" || st.Current == "" {
		t.Fatalf("idle 兜底: %+v", st)
	}
}
