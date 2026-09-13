package store

import (
	"testing"
	"time"
)

// R7 store rules_state：单行约束、往返、seen_max 独立推进。
func TestRulesStateRoundTrip(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 无记录：ok=false
	if _, ok, err := s.LoadRulesState(); ok || err != nil {
		t.Fatalf("empty: ok=%v err=%v, want false/nil", ok, err)
	}

	at := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	row := RulesRefreshRow{
		SeenMax:             11,
		LastAttemptAt:       at,
		LastOKAt:            at.Add(time.Minute),
		LastResult:          "ok",
		LastDetail:          "v11 via A",
		BackoffUntil:        time.Time{},
		ConsecutiveFailures: 0,
		Attempts:            3,
		Successes:           1,
		Rejects:             2,
	}
	if err := s.SaveRulesState(row); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok, err := s.LoadRulesState()
	if !ok || err != nil {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if got.SeenMax != 11 || got.LastResult != "ok" || got.LastDetail != "v11 via A" ||
		got.Attempts != 3 || got.Successes != 1 || got.Rejects != 2 ||
		got.ConsecutiveFailures != 0 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if !got.LastAttemptAt.Equal(at) || !got.LastOKAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("times: attempt=%v ok=%v", got.LastAttemptAt, got.LastOKAt)
	}
	if !got.BackoffUntil.IsZero() {
		t.Fatalf("backoff should be zero, got %v", got.BackoffUntil)
	}

	// 覆盖写（单行 UPSERT 语义）
	row2 := row
	row2.SeenMax = 12
	row2.LastResult = "fetch_err"
	row2.ConsecutiveFailures = 1
	row2.BackoffUntil = at.Add(time.Hour)
	if err := s.SaveRulesState(row2); err != nil {
		t.Fatalf("save2: %v", err)
	}
	got2, ok2, err := s.LoadRulesState()
	if !ok2 || err != nil {
		t.Fatalf("reload: ok=%v err=%v", ok2, err)
	}
	if got2.SeenMax != 12 || got2.LastResult != "fetch_err" ||
		got2.ConsecutiveFailures != 1 || !got2.BackoffUntil.Equal(at.Add(time.Hour)) {
		t.Fatalf("overwrite mismatch: %+v", got2)
	}
	// 覆盖写后计数与时间保留
	if got2.Attempts != 3 || got2.LastOKAt.IsZero() {
		t.Fatalf("overwrite lost fields: %+v", got2)
	}
}

// SetRulesSeenMax 只推进 seen_max 列，不触碰其他字段（A7 磁盘损坏场景
// 下装配层在 Apply 之外的独立推进路径）。
func TestRulesSeenMaxIndependent(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.SetRulesSeenMax(15); err != nil {
		t.Fatalf("set on empty row: %v", err)
	}
	row, ok, err := s.LoadRulesState()
	if !ok || err != nil {
		t.Fatalf("load after set: ok=%v err=%v", ok, err)
	}
	if row.SeenMax != 15 {
		t.Fatalf("seen_max=%d, want 15", row.SeenMax)
	}
	if row.LastResult != "" {
		t.Fatalf("last_result should be empty, got %q", row.LastResult)
	}

	// 先写全行，再独立推进 seen_max
	at := time.Now().UTC()
	if err := s.SaveRulesState(RulesRefreshRow{
		SeenMax: 10, LastResult: "ok", LastAttemptAt: at, Attempts: 1, Successes: 1,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.SetRulesSeenMax(20); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, _, _ := s.LoadRulesState()
	if got.SeenMax != 20 {
		t.Fatalf("seen_max=%d, want 20", got.SeenMax)
	}
	if got.LastResult != "ok" || got.Attempts != 1 {
		t.Fatalf("other fields clobbered: %+v", got)
	}
}

// 零值时间不落库成 0001-01-01（可空列语义：零值 = NULL = 读回零值）。
func TestRulesStateZeroTimes(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.SaveRulesState(RulesRefreshRow{SeenMax: 9}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok, _ := s.LoadRulesState()
	if !ok {
		t.Fatal("row missing")
	}
	if !got.LastAttemptAt.IsZero() || !got.LastOKAt.IsZero() || !got.BackoffUntil.IsZero() {
		t.Fatalf("times not zero: %+v", got)
	}
}
