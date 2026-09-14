package store

import (
	"database/sql"
	"fmt"
	"time"
)

// config（P4/D6）：flags 持久化。serve 启动合并优先级：
//
//	显式 CLI flag（flag.Visit 判定） > 持久化值（本表） > 默认值
//
// key 集由装配层白名单管控（cdn/rules_url/rules_interval/listen/
// doctor_interval），本层只做通用 KV——语义校验（duration/回环 listen）
// 归装配层，两层职责分离。
//
// 写路径：POST /api/config（GUI Settings）与 `ghydra config set`。
// 同 rules_state 的低频同步写模式（刷新级频率，不值得异步化）。

const configSchema = `
CREATE TABLE IF NOT EXISTS config (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0  -- unix ms
);`

func init() { schemaExtras = append(schemaExtras, configSchema) }

// ConfigSet 写入（UPSERT）。空 key 拒绝（装配层 bug 防线）；空 value
// 合法（= 显式置空，如 cdn 置空关 B 通道）。
func (s *Store) ConfigSet(key, value string) error {
	if key == "" {
		return fmt.Errorf("config: key 不能为空")
	}
	now := time.Now().UnixMilli()
	_, err := s.db.Exec(`INSERT INTO config(key, value, updated_at) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, now)
	if err != nil {
		return fmt.Errorf("config: set %s: %w", key, err)
	}
	return nil
}

// ConfigGet 读取。不存在返回 ok=false（区分「未持久化」与「置空」）。
func (s *Store) ConfigGet(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("config: get %s: %w", key, err)
	}
	return v, true, nil
}

// ConfigUpdatedAt 最近写入时间（unix ms；不存在返回 0）。
func (s *Store) ConfigUpdatedAt(key string) (int64, error) {
	var ts sql.NullInt64
	err := s.db.QueryRow(`SELECT updated_at FROM config WHERE key = ?`, key).Scan(&ts)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("config: updated_at %s: %w", key, err)
	}
	return ts.Int64, nil
}

// ConfigList 全量键值（ serve 启动合并一次读全表）。
func (s *Store) ConfigList() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM config`)
	if err != nil {
		return nil, fmt.Errorf("config: list: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("config: list scan: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ConfigDelete 删除（不存在不报错）。
func (s *Store) ConfigDelete(key string) error {
	_, err := s.db.Exec(`DELETE FROM config WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("config: delete %s: %w", key, err)
	}
	return nil
}

// ConfigCount 行数（测试/诊断用）。
func (s *Store) ConfigCount() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM config`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
