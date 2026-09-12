package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreBasics(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 异步写 → flush（Close 前手动等一拍：写循环 2s tick，这里直接等待）
	s.UpdateLastGood("github.com", "1.1.1.1:443", 0.31, 155)
	s.AppendProbe("github.com", "1.1.1.1:443", true, 155, "passive")
	s.AppendProbe("github.com", "2.2.2.2:443", false, 5000, "passive")

	// 等写循环消化（最多 3s）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m, err := s.LastGood()
		if err != nil {
			t.Fatal(err)
		}
		if len(m) == 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	m, err := s.LastGood()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := m["github.com"]
	if !ok {
		t.Fatalf("last_good 应有 github.com: %v", m)
	}
	if e.IP != "1.1.1.1:443" || e.RTTMS != 155 {
		t.Fatalf("last_good 内容错: %+v", e)
	}
}

func TestLastGoodScoreUpdateCondition(t *testing.T) {
	// 条件更新：只有 score 更优或记录老化（>24h）才覆盖
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.UpdateLastGood("github.com", "1.1.1.1:443", 0.31, 155)
	waitFor(t, s, "github.com", "1.1.1.1:443")

	// 更差的 score 不应覆盖
	s.UpdateLastGood("github.com", "9.9.9.9:443", 0.9, 900)
	waitFor(t, s, "github.com", "1.1.1.1:443")

	// 更优的 score 应覆盖
	s.UpdateLastGood("github.com", "2.2.2.2:443", 0.1, 90)
	waitFor(t, s, "github.com", "2.2.2.2:443")
}

func waitFor(t *testing.T, s *Store, domain, wantIP string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m, _ := s.LastGood()
		if e, ok := m[domain]; ok && e.IP == wantIP {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	m, _ := s.LastGood()
	t.Fatalf("等 %s=%s 超时, got %v", domain, wantIP, m[domain])
}

func TestStoreRestartRecovery(t *testing.T) {
	// 断电重启：数据落盘可恢复
	path := filepath.Join(t.TempDir(), "t.db")
	func() {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		s.UpdateLastGood("github.com", "1.1.1.1:443", 0.31, 155)
		s.AppendProbe("github.com", "1.1.1.1:443", true, 155, "passive")
		// Close 会 flush 在途写
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	m, err := s2.LastGood()
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := m["github.com"]; !ok || e.IP != "1.1.1.1:443" {
		t.Fatalf("重启后 last_good 应恢复: %+v", m)
	}

	// probe_log 同样持久
	var n int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM probe_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("probe_log 行数 = %d, want 1", n)
	}
}

func TestQueueFullDoesNotBlock(t *testing.T) {
	// 写队列满时 UpdateLastGood/AppendProbe 应直接丢弃而非阻塞
	s, err := Open("") // 内存库
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100000; i++ { // 远超 1024 容量
			s.AppendProbe("x.com", "1.2.3.4:443", true, 10, "passive")
			s.UpdateLastGood("x.com", "1.2.3.4:443", 0.1, 10)
		}
	}()
	select {
	case <-done:
		// 通过：未阻塞
	case <-time.After(5 * time.Second):
		t.Fatal("写队列满时阻塞了热路径")
	}
}

func TestSnapshotRoundtrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, _, ok, _ := s.LoadSnapshot(); ok {
		t.Fatal("初始应无快照")
	}
	if err := s.SaveSnapshot("192.168.1.1:8888", "http://old/pac"); err != nil {
		t.Fatal(err)
	}
	ps, pac, ok, err := s.LoadSnapshot()
	if err != nil || !ok {
		t.Fatalf("快照读回: ok=%v err=%v", ok, err)
	}
	if ps != "192.168.1.1:8888" || pac != "http://old/pac" {
		t.Fatalf("快照内容错: %q %q", ps, pac)
	}
	// 覆盖（同一 id 单行）
	s.SaveSnapshot("", "")
	ps2, pac2, ok2, _ := s.LoadSnapshot()
	if !ok2 || ps2 != "" || pac2 != "" {
		t.Fatalf("覆盖失败: %q %q %v", ps2, pac2, ok2)
	}
	if err := s.DeleteSnapshot(); err != nil {
		t.Fatal(err)
	}
	if _, _, ok3, _ := s.LoadSnapshot(); ok3 {
		t.Fatal("删除后应无快照")
	}
}

func TestDoctorRecordAndSummary(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	s.AppendDoctor(DoctorRecord{RunID: "r1", StartedAt: now, Mode: "direct", Scenario: "web", Name: "web", Target: "https://github.com", OK: true, Reachable: true, Class: "ok", DurationMS: 100, TTFBMS: 80})
	s.AppendDoctor(DoctorRecord{RunID: "r1", StartedAt: now, Mode: "direct", Scenario: "web", Name: "web", Target: "https://github.com", OK: false, Reachable: false, Class: "timeout", DurationMS: 200})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, qerr := s.DoctorSummary(now.Add(-time.Second))
		if qerr != nil {
			t.Fatal(qerr)
		}
		if len(rows) == 1 && rows[0].Checks == 2 {
			if rows[0].Passed != 1 || rows[0].Reachable != 1 {
				t.Fatalf("summary counts: %+v", rows[0])
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("doctor_log 未在时限内落库")
}
