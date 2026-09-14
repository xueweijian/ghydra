package main

// updateapi_test.go —— P4/D7 L1/L2：updateRunner 状态机（fake release server）。
//
// 覆盖：idle 兜底 / check 全链路（HasUpdate+缓存）/ apply 状态转移
// （checking→downloading→…→failed）+ 单飞释放。pending_boot 成功路径由
// selfupdate 包 TestApplyServeModePendingBoot 覆盖（ServeMode 第一段语义）；
// 端到端链路由 smoke_w4p2_p4.sh（p4c）黑盒覆盖。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xueweijian/ghydra/engine/api"
	"github.com/xueweijian/ghydra/engine/get"
	"github.com/xueweijian/ghydra/engine/selfupdate"
)

// plainFetcher 直接 HTTP Fetcher（把 get.Downloader 指向 fake server）。
type plainFetcher struct{ cl *http.Client }

func (p plainFetcher) Get(ctx context.Context, rawURL string, from int64) (*http.Response, error) {
	var body io.Reader
	if from >= 0 {
		body = strings.NewReader("resume-not-needed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, body)
	if err != nil {
		return nil, err
	}
	if from >= 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
	}
	return p.cl.Do(req)
}

// fakeRelServer 最小 Releases API 替身：latest JSON + 资产头（不真供体——
// Check 只看元数据；Apply 的下载必然 404 失败 → failed 路径）。
func fakeRelServer(t *testing.T) *httptest.Server {
	t.Helper()
	arc := "ghydra-" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
	if runtime.GOOS == "windows" {
		arc = "ghydra-" + runtime.GOOS + "-" + runtime.GOARCH + ".zip"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"tag_name":"v1.0.1","prerelease":false,"body":"changelog","html_url":"https://github.com/x/y/releases/tag/v1.0.1","assets":[
			{"name":%q,"browser_download_url":"http://%s/files/%s","size":100},
			{"name":"checksums.txt","browser_download_url":"http://%s/files/checksums.txt","size":64},
			{"name":"checksums.txt.minisig","browser_download_url":"http://%s/files/checksums.txt.minisig","size":200}]}`,
			arc, r.Host, arc, r.Host, r.Host)
	})
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // 下载必败 → failed 转移
	})
	return httptest.NewServer(mux)
}

func testUpdater(t *testing.T, baseURL string) *selfupdate.Updater {
	t.Helper()
	dir := t.TempDir()
	cl := &http.Client{Timeout: 5 * time.Second}
	return &selfupdate.Updater{
		Version:    "1.0.0",
		APIBase:    baseURL + "/repo",
		HTTP:       cl,
		DL:         get.New(get.Config{}, plainFetcher{cl}, nil),
		InstallDir: dir,
		StatePath:  filepath.Join(t.TempDir(), "update.json"),
		Files:      []string{selfupdate.ExeName},
		// hostAllowed 的 override 旁路按 Hostname()（剥端口）匹配。
		TrustedHosts: map[string]bool{"127.0.0.1": true},
		Log:          t.Logf,
	}
}

func TestUpdateRunnerCheckAndCache(t *testing.T) {
	ts := fakeRelServer(t)
	defer ts.Close()
	r := newUpdateRunnerFrom(testUpdater(t, ts.URL), nil)
	if r == nil {
		t.Fatal("runner 构造失败")
	}

	info, err := r.Check()
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !info.HasUpdate || info.Latest != "1.0.1" || info.Current != Version {
		t.Fatalf("check: %+v", info)
	}
	if info.Cached {
		t.Error("首次不应 cached")
	}
	// 5min 缓存命中
	info2, err := r.Check()
	if err != nil || !info2.Cached {
		t.Fatalf("缓存: %+v err=%v", info2, err)
	}
}

func TestUpdateRunnerApplyStateMachine(t *testing.T) {
	ts := fakeRelServer(t)
	defer ts.Close()

	var mu sync.Mutex
	phases := map[string]bool{}
	r := newUpdateRunnerFrom(testUpdater(t, ts.URL), nil)
	// OnPhase 已在 From 里接 setPhase；这里额外记录（重新赋值包装）
	prev := r.updater.OnPhase
	r.updater.OnPhase = func(p string) {
		mu.Lock()
		phases[p] = true
		mu.Unlock()
		prev(p)
	}

	id, err := r.Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if id == "" {
		t.Fatal("apply id 为空")
	}

	// 单飞：在飞期间再 Apply → ErrBusy
	if _, err := r.Apply(); err != api.ErrBusy {
		t.Fatalf("单飞应 409（ErrBusy），得到 %v", err)
	}

	// 等待失败路径落地（下载 404 → failed；10min 上限，5s 兜底）
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if r.Status().State == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	st := r.Status()
	t.Logf("final status: %+v", st)
	if st.State != "failed" {
		t.Fatalf("状态 = %q（应 failed——下载 404）", st.State)
	}
	if st.Error == "" {
		t.Error("failed 态必须带 error 串")
	}
	if st.Target != "1.0.1" {
		t.Errorf("Target = %q", st.Target)
	}
	mu.Lock()
	seen := len(phases) > 0
	mu.Unlock()
	if !seen {
		t.Error("OnPhase 未触发任何阶段")
	}

	// 失败后单飞释放：再次 Apply 应能启动（不再 409）
	if _, err := r.Apply(); err == api.ErrBusy {
		t.Fatal("失败后单飞应已释放")
	}
	// 等第二次落地（避免 goroutine 泄漏影响后续）
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if r.Status().State == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestUpdateRunnerNilUpdater(t *testing.T) {
	if r := newUpdateRunnerFrom(nil, nil); r != nil {
		t.Fatal("nil updater 应返回 nil runner（503 面）")
	}
}
