package selfupdate

// L2 集成（设计 §7 用例 9-15）：fake Release server（httptest 起 Releases
// API + 资产服务）驱动 全链路 check→download→verify→swap→自检→对账，
// 以及 U1/U2/U4 攻击矩阵、崩溃恢复、自动回滚。
// "新版二进制" = testdata/fakebin 编译出的真子进程（env 驱动行为）。

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/xueweijian/ghydra/engine/get"
	"github.com/xueweijian/ghydra/engine/internal/minisign"
	"github.com/xueweijian/ghydra/engine/rules"
)

// fakebin 编译产物路径（TestMain 填充）。
var fakebinPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ghydra-fakebin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	name := "fakebin"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-o", out, "./testdata/fakebin")
	if combined, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build fakebin: %v\n%s", err, combined)
		os.Exit(1)
	}
	fakebinPath = out
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// ---- fake 组件 ----

// fakeDaemon daemon 编排替身： Alive 由测试翻转；Start 后运行版本 =
// nextVersion（模拟"新 daemon 跑新 exe"）；WaitVersion 对账该值。
type fakeDaemon struct {
	mu          sync.Mutex
	alive       bool
	stops       int
	starts      int
	nextVersion string
	waitErr     error
}

func (d *fakeDaemon) Alive() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.alive
}

func (d *fakeDaemon) Stop() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stops++
	d.alive = false
	return nil
}

func (d *fakeDaemon) Start() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.starts++
	d.alive = true
	return nil
}

func (d *fakeDaemon) WaitVersion(ctx context.Context, want string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.waitErr != nil {
		return d.waitErr
	}
	if d.nextVersion != want {
		return fmt.Errorf("daemon 版本 %q ≠ 期望 %q", d.nextVersion, want)
	}
	return nil
}

func (d *fakeDaemon) counts() (stops, starts int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stops, d.starts
}

// plainFetcher 直接 HTTP Fetcher（测试把 get.Downloader 指到 fake server）。
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

// harness L2 测试台。
type harness struct {
	srv            *httptest.Server
	dir            string // 安装目录
	stateDir       string
	pub            ed25519.PublicKey
	sk             minisign.SecretKey
	daemon         *fakeDaemon
	updater        *Updater
	mu             sync.Mutex
	assets         map[string][]byte // server 上的资产（可被测试替换/篡改）
	wrongSizeAsset string            // U4：该资产在 API 元数据里尺寸报错
}

// newHarness 装配：TLS fake server（/repo/releases/latest + /files/*）+
// 已安装的 "v1.0.0"（fakebin 拷贝）+ 默认无 daemon。
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{assets: map[string][]byte{}}

	sk, err := minisign.ParseSecretKeyFile("testdata/test.key")
	if err != nil {
		t.Fatal(err)
	}
	h.sk = sk
	pub, _, err := rules.ParsePublicKey(mustRead(t, "testdata/test.pub"))
	if err != nil {
		t.Fatal(err)
	}
	h.pub = pub

	h.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(h.releaseJSONLocked())
		case strings.HasPrefix(r.URL.Path, "/files/"):
			name := strings.TrimPrefix(r.URL.Path, "/files/")
			data, ok := h.assets[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			_, _ = w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.srv.Close)

	h.dir = t.TempDir()
	h.stateDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(h.dir, ExeName), fakebinBytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	h.daemon = &fakeDaemon{nextVersion: "1.0.0"}
	h.updater = &Updater{
		Version:           "1.0.0",
		APIBase:           h.srv.URL + "/repo",
		HTTP:              h.srv.Client(),
		DL:                get.New(get.Config{}, plainFetcher{h.srv.Client()}, nil),
		InstallDir:        h.dir,
		StatePath:         filepath.Join(h.stateDir, StateFileName),
		Files:             []string{ExeName},
		Daemon:            h.daemon,
		Pub:               h.pub,
		Log:               func(f string, a ...any) { t.Logf(f, a...) },
		TrustedHosts:      map[string]bool{"127.0.0.1": true},
		SelfCheckExtraEnv: []string{"FAKE_VERSION=1.0.1"},
	}
	return h
}

func fakebinBytes() []byte {
	b, err := os.ReadFile(fakebinPath)
	if err != nil {
		panic("fakebin 未编译: " + err.Error())
	}
	return b
}

// publishRelease 构造 v1.0.1 的三件套资产并上 server。
// 篡改语义（攻击矩阵）：签名链基于**原始**内容构建，篡改只发生在
// server 侧资产字节——模拟"传输/源侧被换"，防线必须当场识破。
func (h *harness) publishRelease(t *testing.T, tamperAsset, tamperChecksums, wrongSize bool) {
	t.Helper()
	newExe := fakebinBytes() // "新版" 与旧版同字节也行：版本由 env 决定
	archiveName := "ghydra-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		archiveName += ".zip"
	} else {
		archiveName += ".tar.gz"
	}
	archive := buildArchive(t, archiveName, map[string][]byte{ExeName: newExe})
	sum := sha256.Sum256(archive)
	csBody := fmt.Sprintf("%s  %s\n%s  checksums.txt\n",
		hex.EncodeToString(sum[:]), archiveName,
		strings.Repeat("a", 64))
	sig, err := minisign.SignDetached(h.sk, []byte(csBody), "untrusted comment: ghydra release", "ghydra release v1.0.1")
	if err != nil {
		t.Fatal(err)
	}
	serveArc := archive
	if tamperAsset && len(serveArc) > 4 {
		serveArc = append([]byte{}, serveArc...)
		serveArc[3] ^= 0xff
	}
	serveCS := []byte(csBody)
	if tamperChecksums {
		serveCS = bytes.Replace(serveCS, []byte("checksums.txt\n"), []byte("checksums.txX\n"), 1)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.assets[archiveName] = serveArc
	h.assets["checksums.txt"] = serveCS
	h.assets["checksums.txt.minisig"] = sig
	if wrongSize {
		// API 元数据里把 archive 尺寸报错（U4）
		h.wrongSizeAsset = archiveName
	}
}

// releaseJSONLocked 动态产 Releases API JSON（含 wrongSize 注入点）。
// ⚠️ 调用方必须已持 h.mu（handler 路径）。
func (h *harness) releaseJSONLocked() map[string]any {
	wrong := h.wrongSizeAsset
	archiveName := "ghydra-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		archiveName += ".zip"
	} else {
		archiveName += ".tar.gz"
	}
	base := h.srv.URL + "/files/"
	size := int64(len(h.assets[archiveName]))
	if wrong == archiveName {
		size += 4096
	}
	assets := []map[string]any{}
	for _, name := range []string{archiveName, "checksums.txt", "checksums.txt.minisig"} {
		s := int64(len(h.assets[name]))
		if wrong == name {
			s += 4096
		}
		assets = append(assets, map[string]any{
			"name": name, "size": s, "browser_download_url": base + name,
		})
	}
	_ = size
	return map[string]any{
		"tag_name": "v1.0.1", "draft": false, "prerelease": false,
		"body": "release notes", "assets": assets,
	}
}

func (h *harness) check(t *testing.T, opts SelectOpts) Plan {
	t.Helper()
	plan, err := h.updater.Check(context.Background(), opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return plan
}

// ---- 用例 ----

// L2-9 happy path：check→download→verify→swap→自检确认全绿。
func TestApplyHappyPath(t *testing.T) {
	h := newHarness(t)
	h.publishRelease(t, false, false, false)

	plan := h.check(t, SelectOpts{})
	if err := h.updater.ApplyPlan(context.Background(), plan); err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}

	// exe = 新版（fakebin 同字节，以状态与 old 凭证断言语义）
	got, _ := os.ReadFile(filepath.Join(h.dir, ExeName))
	old, _ := os.ReadFile(filepath.Join(h.dir, OldName))
	if len(got) == 0 || len(old) == 0 || bytes.Equal(got, old) == false && len(got) != len(old) {
		// 内容层面：两个 fakebin 字节相同——改用"文件在场 + 状态"断言
	}
	if _, err := os.Stat(filepath.Join(h.dir, ExeName)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, OldName)); err != nil {
		t.Error("old 凭证应在场")
	}
	st, exists, err := LoadState(h.updater.StatePath)
	if err != nil || !exists {
		t.Fatalf("状态: %v %v", err, exists)
	}
	if !st.Confirmed || st.PendingVersion != "1.0.1" {
		t.Errorf("状态 = %+v（应 confirmed + pending 1.0.1）", st)
	}
	// daemon 未在跑（默认）→ 不应重启
	if stops, starts := h.daemon.counts(); stops != 0 || starts != 0 {
		t.Errorf("无 daemon 场景不应编排重启: stops=%d starts=%d", stops, starts)
	}
	// staging 清理留给下次 BootHook；此处不残留 minisig/checksums 于安装根
	for _, name := range []string{"checksums.txt", "checksums.txt.minisig"} {
		if _, err := os.Stat(filepath.Join(h.dir, name)); err == nil {
			t.Errorf("%s 不应落在安装根", name)
		}
	}
}

// L2-14 daemon 对账：更新前在跑 → 新 daemon 起来 + 版本对上。
func TestApplyDaemonReconcile(t *testing.T) {
	h := newHarness(t)
	h.publishRelease(t, false, false, false)
	h.daemon.mu.Lock()
	h.daemon.alive = true
	h.daemon.nextVersion = "1.0.1" // 新 daemon 跑新 exe
	h.daemon.mu.Unlock()

	plan := h.check(t, SelectOpts{})
	if err := h.updater.ApplyPlan(context.Background(), plan); err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	stops, starts := h.daemon.counts()
	if stops < 1 || starts < 1 {
		t.Errorf("应停旧起新: stops=%d starts=%d", stops, starts)
	}
	if !h.daemon.Alive() {
		t.Error("新 daemon 应存活")
	}
	st, _, _ := LoadState(h.updater.StatePath)
	if st == nil || !st.Confirmed {
		t.Errorf("daemon 对账通过应确认: %+v", st)
	}
}

// L2-10 U1 双防线：资产篡改 → sha256 拒；checksums 篡改 → minisig 拒。
func TestApplyTamperAsset(t *testing.T) {
	h := newHarness(t)
	h.publishRelease(t, true, false, false)
	plan := h.check(t, SelectOpts{})
	err := h.updater.ApplyPlan(context.Background(), plan)
	if !errors.Is(err, ErrChecksumMismatch) && !strings.Contains(fmt.Sprint(err), "校验失败") {
		t.Fatalf("资产篡改应拒: %v", err)
	}
	h.assertInstallIntact(t)
}

func TestApplyTamperChecksums(t *testing.T) {
	h := newHarness(t)
	h.publishRelease(t, false, true, false)
	plan := h.check(t, SelectOpts{})
	err := h.updater.ApplyPlan(context.Background(), plan)
	if err == nil || !strings.Contains(fmt.Sprint(err), "验签失败") {
		t.Fatalf("checksums 篡改应 minisig 拒: %v", err)
	}
	h.assertInstallIntact(t)
}

// L2-12 U4 尺寸不符拒。
func TestApplySizeMismatch(t *testing.T) {
	h := newHarness(t)
	h.publishRelease(t, false, false, true)
	plan := h.check(t, SelectOpts{})
	err := h.updater.ApplyPlan(context.Background(), plan)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("尺寸不符应 ErrSizeMismatch: %v", err)
	}
	h.assertInstallIntact(t)
}

// L2-11 U2 回滚拒 + 显式降级放行。
func TestCheckNoDowngradeByDefault(t *testing.T) {
	h := newHarness(t)
	h.publishRelease(t, false, false, false)
	h.updater.Version = "1.2.0"
	_, err := h.updater.Check(context.Background(), SelectOpts{})
	if !errors.Is(err, ErrNoUpdate) {
		t.Fatalf("默认应拒回滚: %v", err)
	}
	if _, err := h.updater.Check(context.Background(), SelectOpts{AllowDowngrade: true}); err != nil {
		t.Fatalf("显式降级应放行: %v", err)
	}
}

// L2-15 自动回滚（Apply 内联路径）：新版 = 坏二进制 → 立即回滚 +
// daemon 回旧版 + bad_version 生效。
func TestApplyBrokenBinaryRollsBack(t *testing.T) {
	h := newHarness(t)
	h.publishRelease(t, false, false, false)
	h.updater.SelfCheckExtraEnv = []string{"FAKE_MODE=broken"}
	h.daemon.mu.Lock()
	h.daemon.alive = true
	h.daemon.nextVersion = "1.0.0" // 回滚后旧 daemon
	h.daemon.mu.Unlock()

	plan := h.check(t, SelectOpts{})
	err := h.updater.ApplyPlan(context.Background(), plan)
	if err == nil {
		t.Fatal("坏新版 Apply 必须报错")
	}
	// 旧版恢复正身
	got, _ := os.ReadFile(filepath.Join(h.dir, ExeName))
	if len(got) == 0 {
		t.Fatal("回滚后 exe 缺失")
	}
	// 坏版留档
	if _, err := os.Stat(filepath.Join(h.dir, BadName)); err != nil {
		t.Error("坏版应留档 .bad")
	}
	// daemon 回旧版且活着
	if !h.daemon.Alive() {
		t.Error("回滚后旧 daemon 应存活")
	}
	st, _, _ := LoadState(h.updater.StatePath)
	if st == nil || !st.IsBad("1.0.1") {
		t.Errorf("bad_version 应记录: %+v", st)
	}
	// 同版本再 check → 跳过
	if _, err := h.updater.Check(context.Background(), SelectOpts{}); !errors.Is(err, ErrBadVersion) {
		t.Errorf("坏版本再 check 应 ErrBadVersion: %v", err)
	}
}

// L2-15b BootHook 路径 ×3 自动回滚（崩溃在 swap 与确认之间）。
func TestBootHookTripleFailureRollsBack(t *testing.T) {
	h := newHarness(t)
	// 人工制造"swap 完成、未确认、新版坏"状态
	newExe := fakebinBytes()
	oldExe := append([]byte("old-v1.0.0-bin"), newExe...)
	if err := os.WriteFile(filepath.Join(h.dir, OldName), oldExe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, ExeName), newExe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(h.updater.StatePath, &State{PendingVersion: "1.0.1", DaemonWasRunning: false}); err != nil {
		t.Fatal(err)
	}
	h.updater.SelfCheckExtraEnv = []string{"FAKE_MODE=broken"}

	for i := 1; i <= 2; i++ {
		err := h.updater.BootHook(false)
		if err != nil {
			t.Fatalf("第 %d 次自检失败应静默累积（BootHook 返回 nil）: %v", i, err)
		}
		st, _, _ := LoadState(h.updater.StatePath)
		if st.BootAttempts != i {
			t.Fatalf("attempts = %d，期望 %d", st.BootAttempts, i)
		}
	}
	err := h.updater.BootHook(false) // 第 3 次：回滚
	if err == nil {
		t.Fatal("第 3 次失败应回滚并报错")
	}
	got, _ := os.ReadFile(filepath.Join(h.dir, ExeName))
	if !bytes.Equal(got, oldExe) {
		t.Error("回滚后 exe 应为旧版字节")
	}
	st, _, _ := LoadState(h.updater.StatePath)
	if st == nil || !st.IsBad("1.0.1") || st.PendingVersion != "" {
		t.Errorf("回滚后状态 = %+v", st)
	}
}

// L2-13 崩溃恢复矩阵：交换序列各步中断 → BootHook 清扫 → 旧版可跑。
func TestBootHookCrashRecovery(t *testing.T) {
	t.Run("中断于首rename后", func(t *testing.T) {
		h := newHarness(t)
		os.Remove(filepath.Join(h.dir, ExeName))
		os.WriteFile(filepath.Join(h.dir, OldName), []byte("old-v1"), 0o755)
		os.MkdirAll(filepath.Join(h.dir, StagingDirName), 0o755)
		os.WriteFile(filepath.Join(h.dir, StagingDirName, "half.tmp"), []byte("x"), 0o644)
		SaveState(h.updater.StatePath, &State{PendingVersion: "1.0.1"})

		if err := h.updater.BootHook(false); err != nil {
			t.Fatalf("BootHook: %v", err)
		}
		got, _ := os.ReadFile(filepath.Join(h.dir, ExeName))
		if string(got) != "old-v1" {
			t.Errorf("旧版应恢复: %q", got)
		}
		st, _, _ := LoadState(h.updater.StatePath)
		if st == nil || !st.IsBad("1.0.1") {
			t.Errorf("中断更新应标 bad: %+v", st)
		}
	})
	t.Run("staging残留无状态", func(t *testing.T) {
		h := newHarness(t)
		os.MkdirAll(filepath.Join(h.dir, StagingDirName), 0o755)
		os.WriteFile(filepath.Join(h.dir, StagingDirName, "junk"), []byte("x"), 0o644)
		if err := h.updater.BootHook(false); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(h.dir, StagingDirName)); !os.IsNotExist(err) {
			t.Error("staging 应被清扫")
		}
	})
	t.Run("健康带old凭证", func(t *testing.T) {
		h := newHarness(t)
		os.WriteFile(filepath.Join(h.dir, OldName), []byte("v0"), 0o755)
		if err := h.updater.BootHook(false); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(h.dir, ExeName))
		if len(got) == 0 {
			t.Error("健康 exe 不应被动")
		}
	})
}

// L2 附加：BootHook 自检通过 → 确认（swap 后崩溃、下次启动补确认）。
func TestBootHookConfirmsAfterCrash(t *testing.T) {
	h := newHarness(t)
	h.updater.SelfCheckExtraEnv = []string{"FAKE_VERSION=1.0.1"}
	SaveState(h.updater.StatePath, &State{PendingVersion: "1.0.1"})
	if err := h.updater.BootHook(false); err != nil {
		t.Fatalf("BootHook: %v", err)
	}
	st, _, _ := LoadState(h.updater.StatePath)
	if st == nil || !st.Confirmed {
		t.Errorf("应确认: %+v", st)
	}
}

// L2 附加：手动 Rollback 子命令路径。
func TestManualRollback(t *testing.T) {
	h := newHarness(t)
	h.publishRelease(t, false, false, false)
	plan := h.check(t, SelectOpts{})
	if err := h.updater.ApplyPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if err := h.updater.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	st, _, _ := LoadState(h.updater.StatePath)
	if st == nil || !st.IsBad("1.0.1") {
		t.Errorf("手动回滚也应记 bad: %+v", st)
	}
}

// assertInstallIntact 攻击拒后安装目录零破坏。
func (h *harness) assertInstallIntact(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(h.dir, ExeName))
	if err != nil {
		t.Fatal("攻击拒后 exe 必须原样在场:", err)
	}
	if !bytes.Equal(got, fakebinBytes()) {
		t.Error("攻击拒后 exe 字节被改动")
	}
	if _, err := os.Stat(filepath.Join(h.dir, OldName)); err == nil {
		t.Error("攻击拒后不应产生 old 凭证（未进入交换）")
	}
}

// ---- archive 构造 ----

func buildArchive(t *testing.T, name string, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if strings.HasSuffix(name, ".zip") {
		zw := zip.NewWriter(&buf)
		for fname, data := range files {
			w, err := zw.Create(fname)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(data); err != nil {
				t.Fatal(err)
			}
		}
		zw.Close()
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for fname, data := range files {
		hdr := &tar.Header{Name: fname, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// 编译期断言接口实现（防漂移）。
var _ DaemonControl = (*fakeDaemon)(nil)
var _ get.Fetcher = plainFetcher{}
