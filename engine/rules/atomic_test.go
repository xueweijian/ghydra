package rules

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// atomicEnv 在临时目录构造一对真实可验签的规则（版本 version）。
func atomicEnv(t *testing.T, s *testSigner, version int64) (dir, base string, data, sig []byte) {
	t.Helper()
	dir = t.TempDir()
	base = "current.json"
	data = []byte(`{"schema_version":1,"version":` + itoa(version) + `,"domains":["github.com"]}`)
	sig = s.signFile(data, "ghydra-rules v"+itoa(version))
	if err := SavePairAtomic(dir, base, data, sig); err != nil {
		t.Fatalf("SavePairAtomic: %v", err)
	}
	return dir, base, data, sig
}

// TestAtomicRoundtrip 写后立即可读、逐字节一致、无 tmp 残留。
func TestAtomicRoundtrip(t *testing.T) {
	s := newTestSigner()
	dir, base, data, sig := atomicEnv(t, s, 42)
	got, gotSig, err := LoadPair(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) || string(gotSig) != string(sig) {
		t.Fatal("roundtrip mismatch")
	}
	if err := s.verifyFile(got, gotSig); err != nil {
		t.Fatalf("loaded pair fails verification: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("tmp residue: %s", e.Name())
		}
	}
}

// TestAtomicCrashResidue 模拟崩溃残留 .tmp：不影响加载与下一次写入。
func TestAtomicCrashResidue(t *testing.T) {
	s := newTestSigner()
	dir, base, data, _ := atomicEnv(t, s, 42)
	os.WriteFile(filepath.Join(dir, base+".tmp"), []byte(`{"schema_version":1,"vers`), 0o644)
	os.WriteFile(filepath.Join(dir, base+".minisig.tmp"), []byte("garbage"), 0o644)
	got, gotSig, err := LoadPair(dir, base)
	if err != nil || string(got) != string(data) || s.verifyFile(got, gotSig) != nil {
		t.Fatalf("residue broke load: %v", err)
	}
	if err := SavePairAtomic(dir, base, data, s.signFile(data, "rewritten")); err != nil {
		t.Fatalf("rewrite after residue: %v", err)
	}
	if _, _, err := LoadPair(dir, base); err != nil {
		t.Fatalf("load after rewrite: %v", err)
	}
}

// TestAtomicMissingSig 缺 sig 整体失败（成对语义 → provider 回退地板）。
func TestAtomicMissingSig(t *testing.T) {
	s := newTestSigner()
	dir, base, _, _ := atomicEnv(t, s, 42)
	os.Remove(filepath.Join(dir, base+".minisig"))
	if _, _, err := LoadPair(dir, base); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
}

// TestAtomicConcurrentReadWrite 写侧轮换版本、读侧并发加载：
// 读到的对要么完整可验（旧版或新版），要么验签失败——撕裂对是两文件
// rename 窗口的预期产物（设计 §4），防线是加载侧成对验签（provider 行为），
// 而不是不存在的跨文件原子事务。运行时热生效路径读内存快照，不读磁盘，
// 撕裂窗只影响「拉取落盘后同 goroutine 的一次性验证」——顺序执行无窗。
func TestAtomicConcurrentReadWrite(t *testing.T) {
	s := newTestSigner()
	dir, base, d42, s42 := atomicEnv(t, s, 42)
	d43 := []byte(`{"schema_version":1,"version":43,"domains":["github.com"]}`)
	s43 := s.signFile(d43, "ghydra-rules v43")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // 读侧：模拟 provider 加载（读对 + 验签）
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, gotSig, err := LoadPair(dir, base)
			time.Sleep(500 * time.Microsecond) // 读侧非紧循环（Windows 共享窗口照应）
			if err != nil {
				t.Errorf("reader load: %v", err)
				return
			}
			err = s.verifyFile(got, gotSig)
			switch {
			case err == nil && string(got) != string(d42) && string(got) != string(d43):
				t.Error("verified pair with unknown content")
				return
			case err == nil && string(gotSig) != string(s42) && string(gotSig) != string(s43):
				t.Error("verified pair with unknown sig")
				return
			default:
				// 撕裂对（验签失败）或完整对：皆在防线语义内。
			}
		}
	}()
	for i := 0; i < 50; i++ { // 写侧轮换制造撕裂窗
		if i%2 == 0 {
			if err := SavePairAtomic(dir, base, d43, s43); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := SavePairAtomic(dir, base, d42, s42); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(time.Millisecond) // 真实节奏：provider 落盘是低频事件，非紧循环
	}
	close(stop)
	wg.Wait()
	// 收尾不变量：静置后必然收敛到完整可验对。
	got, gotSig, err := LoadPair(dir, base)
	if err != nil || s.verifyFile(got, gotSig) != nil {
		t.Fatalf("final pair broken: %v", err)
	}
}
