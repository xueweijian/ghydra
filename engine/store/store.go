// Package store 实现 GHydra 的本地持久化（M1-W2）：
// last_good（重启恢复的种子 IP）+ probe_log（W4 SLA/doctor 的粮）。
//
// 写路径全异步（channel + 单写 goroutine）：热路径（Report → store）
// 绝不阻塞，掉电最多丢窗口内数据。读路径同步（启动恢复 / status）。
package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// LastGoodEntry last_good 表的一行。
type LastGoodEntry struct {
	IP      string
	Score   float64
	RTTMS   float64
	Updated time.Time
}

// probeRow probe_log 的一行（追加型）。
type probeRow struct {
	domain, ip string
	ok         bool
	rttMS      float64
	source     string // passive | active | bootstrap
}

// Store SQLite 存储。构造用 Open；Close 等待在途写完成。
type Store struct {
	db *sql.DB

	ch   chan probeRow // 探测日志异步队列
	last chan lastGoodOp
	wg   sync.WaitGroup
	stop chan struct{}
}

type lastGoodOp struct {
	domain, ip string
	score, rtt float64
}

// Open 打开（或创建）数据库并启动写 goroutine。path 为空时用内存库
// （测试用）。父目录自动创建。
func Open(path string) (*Store, error) {
	dsn := "file::memory:?cache=shared"
	if path != "" {
		if err := mkdirAll(filepath.Dir(path)); err != nil {
			return nil, err
		}
		// busy_timeout：单写 goroutine 下竞争罕见，保险值
		dsn = "file:" + path + "?_pragma=busy_timeout(3000)&_pragma=synchronous(NORMAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2) // WAL 下读写并发安全；限制连接防句柄泄漏

	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS last_good (
  domain     TEXT PRIMARY KEY,
  ip         TEXT NOT NULL,
  score      REAL NOT NULL,
  rtt_ms     REAL NOT NULL,
  updated_at INTEGER NOT NULL  -- unix ms
);
CREATE TABLE IF NOT EXISTS probe_log (
  id     INTEGER PRIMARY KEY AUTOINCREMENT,
  domain TEXT NOT NULL, ip TEXT NOT NULL,
  ok     INTEGER NOT NULL, rtt_ms REAL NOT NULL,
  source TEXT NOT NULL, ts INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_probe_ts ON probe_log(ts);`); err != nil {
		db.Close()
		return nil, err
	}

	s := &Store{
		db:   db,
		ch:   make(chan probeRow, 1024),
		last: make(chan lastGoodOp, 64),
		stop: make(chan struct{}),
	}
	s.wg.Add(1)
	go s.writeLoop()
	return s, nil
}

// writeLoop 单写 goroutine：批量落盘，掉电最多丢队列内数据。
func (s *Store) writeLoop() {
	defer s.wg.Done()
	flush := func() {
		for {
			select {
			case op := <-s.last:
				s.execLastGood(op)
				continue
			case r := <-s.ch:
				s.execProbe(r)
				continue
			default:
				return
			}
		}
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			flush()
			return
		case op := <-s.last:
			s.execLastGood(op)
		case r := <-s.ch:
			s.execProbe(r)
		case <-tick.C: // 空闲批量刷（队列攒批后一次事务）
			flush()
		}
	}
}

func (s *Store) execLastGood(op lastGoodOp) {
	nowMS := time.Now().UnixMilli()
	// 条件更新（设计 §3.6）：新 score 优于现存，或现存已老化（>24h）
	_, err := s.db.Exec(`INSERT INTO last_good(domain, ip, score, rtt_ms, updated_at)
VALUES(?,?,?,?,?)
ON CONFLICT(domain) DO UPDATE SET ip=excluded.ip, score=excluded.score,
  rtt_ms=excluded.rtt_ms, updated_at=excluded.updated_at
WHERE excluded.score < last_good.score OR last_good.updated_at < ?`,
		op.domain, op.ip, op.score, op.rtt, nowMS, nowMS-24*3600*1000)
	_ = err // 异步路径：失败不回流热路径；writeLoop 可加日志
}

func (s *Store) execProbe(r probeRow) {
	s.db.Exec(`INSERT INTO probe_log(domain, ip, ok, rtt_ms, source, ts)
VALUES(?,?,?,?,?,?)`,
		r.domain, r.ip, b2i(r.ok), r.rttMS, r.source, time.Now().UnixMilli())
}

// --- 异步写接口（非阻塞；队列满则丢弃——探测日志可容忍） ---

// UpdateLastGood 异步更新域名的最佳 IP。
func (s *Store) UpdateLastGood(domain, ip string, score, rttMS float64) {
	select {
	case s.last <- lastGoodOp{domain, ip, score, rttMS}:
	default:
	}
}

// AppendProbe 异步追加探测记录。source: passive/active/bootstrap。
func (s *Store) AppendProbe(domain, ip string, ok bool, rttMS float64, source string) {
	select {
	case s.ch <- probeRow{domain, ip, ok, rttMS, source}:
	default:
	}
}

// --- 同步读接口 ---

// LastGood 读全部 last_good（启动恢复种子）。
func (s *Store) LastGood() (map[string]LastGoodEntry, error) {
	rows, err := s.db.Query(`SELECT domain, ip, score, rtt_ms, updated_at FROM last_good`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]LastGoodEntry)
	for rows.Next() {
		var domain string
		var e LastGoodEntry
		var ts int64
		if err := rows.Scan(&domain, &e.IP, &e.Score, &e.RTTMS, &ts); err != nil {
			return nil, err
		}
		e.Updated = time.UnixMilli(ts)
		out[domain] = e
	}
	return out, rows.Err()
}

// Close 停写并关库（在途数据 flush 后返回）。
func (s *Store) Close() error {
	close(s.stop)
	s.wg.Wait()
	return s.db.Close()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func mkdirAll(dir string) error {
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}
