package rules

import "testing"

// TestDecideVersion 全矩阵表驱动（设计 §3 判定表逐行对应）。
func TestDecideVersion(t *testing.T) {
	cases := []struct {
		seenMax, cand int64
		want          Decision
		why           string
	}{
		// 快进 DoS（A4）：合法签名 + 天文版本 → 拒且可观测。
		{42, 1_000_001, RejectFastForward, "just over cap"},
		{42, 999_999_999, RejectFastForward, "astronomical"},
		{0, 1_000_001, RejectFastForward, "cap on first install"},
		// 回滚（A3）。
		{42, 42, RejectRollback, "equal"},
		{42, 41, RejectRollback, "one back"},
		{42, 1, RejectRollback, "to floor"},
		{42, 0, RejectRollback, "zero"},
		{100, 10, RejectRollback, "far back"},
		// 接受。
		{42, 43, Accept, "increment"},
		{42, 1_000_000, Accept, "exactly at cap"},
		{0, 1, Accept, "first install"},
		{1, 2, Accept, "past embedded"},
	}
	for _, c := range cases {
		got, why := DecideVersion(c.seenMax, c.cand)
		if got != c.want {
			t.Errorf("seen=%d cand=%d: got %v want %v (%s)", c.seenMax, c.cand, got, c.want, c.why)
		}
		if why == "" {
			t.Errorf("seen=%d cand=%d: empty reason", c.seenMax, c.cand)
		}
	}
}
