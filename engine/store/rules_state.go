package store

import (
	"database/sql"
	"time"
)

// rules_state（M3-W2 phase 3）：规则拉取管线的持久化状态，单行表。
//
// seen_max 独立于规则文件持久化是本表的安全核心：A7 场景（磁盘规则
// 被删/篡改）下重启回退 L0，内存 seen_max 会回落到 embedded 版——
// 攻击者若同时控制网络即可重放「embedded 版之上的任意旧合法签名版」。
// 装配层取 max(embedded, disk, 本表) 喂 Provider，堵死该重放窗口。
//
// 写路径同步（与 sysproxy/managed 快照同模式）：刷新频率 ≤ 6h 一次
// （手动亦然），低频路径不值得为异步引入「写后立读」的不一致；
// 极端崩溃丢失由磁盘规则对版本兜底（启动取 max）。

// RulesRefreshRow rules_state 单行投影。
type RulesRefreshRow struct {
	SeenMax             int64
	LastAttemptAt       time.Time // 零值 = 从未尝试
	LastOKAt            time.Time // 零值 = 从未成功
	LastResult          string    // ok|fetch_err|sig_rejected|rollback|fast_forward|schema_rejected|size_exceeded
	LastDetail          string
	BackoffUntil        time.Time // 零值 = 无退避
	ConsecutiveFailures int
	Attempts            int
	Successes           int
	Rejects             int
}

// 规则状态表（建库时随主 schema 一起执行；老库走这里的 IF NOT EXISTS）。
const rulesStateSchema = `
CREATE TABLE IF NOT EXISTS rules_state (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  seen_max INTEGER NOT NULL DEFAULT 0,
  last_attempt_at INTEGER,           -- unix ms；NULL = 从未尝试
  last_ok_at INTEGER,                -- unix ms；NULL = 从未成功
  last_result TEXT NOT NULL DEFAULT '',
  last_detail TEXT NOT NULL DEFAULT '',
  backoff_until INTEGER,             -- unix ms；NULL = 无退避
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  successes INTEGER NOT NULL DEFAULT 0,
  rejects INTEGER NOT NULL DEFAULT 0
);`

func init() { schemaExtras = append(schemaExtras, rulesStateSchema) }

// LoadRulesState 读规则状态；无行返回 ok=false。
func (s *Store) LoadRulesState() (RulesRefreshRow, bool, error) {
	var r RulesRefreshRow
	var attemptMS, okMS, backoffMS sql.NullInt64
	err := s.db.QueryRow(`SELECT seen_max, last_attempt_at, last_ok_at, last_result,
last_detail, backoff_until, consecutive_failures, attempts, successes, rejects
FROM rules_state WHERE id = 1`).Scan(
		&r.SeenMax, &attemptMS, &okMS, &r.LastResult, &r.LastDetail,
		&backoffMS, &r.ConsecutiveFailures, &r.Attempts, &r.Successes, &r.Rejects)
	if err == sql.ErrNoRows {
		return RulesRefreshRow{}, false, nil
	}
	if err != nil {
		return RulesRefreshRow{}, false, err
	}
	if attemptMS.Valid {
		r.LastAttemptAt = time.UnixMilli(attemptMS.Int64)
	}
	if okMS.Valid {
		r.LastOKAt = time.UnixMilli(okMS.Int64)
	}
	if backoffMS.Valid {
		r.BackoffUntil = time.UnixMilli(backoffMS.Int64)
	}
	return r, true, nil
}

// SaveRulesState 全行覆盖写（单行 UPSERT）。
func (s *Store) SaveRulesState(r RulesRefreshRow) error {
	_, err := s.db.Exec(`INSERT INTO rules_state(
id, seen_max, last_attempt_at, last_ok_at, last_result, last_detail,
backoff_until, consecutive_failures, attempts, successes, rejects)
VALUES(1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  seen_max = excluded.seen_max,
  last_attempt_at = excluded.last_attempt_at,
  last_ok_at = excluded.last_ok_at,
  last_result = excluded.last_result,
  last_detail = excluded.last_detail,
  backoff_until = excluded.backoff_until,
  consecutive_failures = excluded.consecutive_failures,
  attempts = excluded.attempts,
  successes = excluded.successes,
  rejects = excluded.rejects`,
		r.SeenMax, msOrNull(r.LastAttemptAt), msOrNull(r.LastOKAt),
		r.LastResult, r.LastDetail, msOrNull(r.BackoffUntil),
		r.ConsecutiveFailures, r.Attempts, r.Successes, r.Rejects)
	return err
}

// SetRulesSeenMax 只推进 seen_max 列（不动其他字段）：装配层在
// Apply 成功路径之外的独立推进（如启动时对齐磁盘版本）。
func (s *Store) SetRulesSeenMax(v int64) error {
	_, err := s.db.Exec(`INSERT INTO rules_state(id, seen_max) VALUES(1, ?)
ON CONFLICT(id) DO UPDATE SET seen_max = excluded.seen_max`, v)
	return err
}

func msOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}
