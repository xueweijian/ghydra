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

// DoctorRecord 是一次 doctor 子检查的持久化投影。使用基础类型，
// 避免 store 依赖 probe 包（probe 本身只负责测量）。
type DoctorRecord struct {
	RunID      string
	StartedAt  time.Time
	Mode       string
	Scenario   string
	Name       string
	Target     string
	OK         bool
	Reachable  bool
	Status     int
	Class      string
	DurationMS float64
	TTFBMS     float64
	Bytes      int64
	RateBPS    float64
}

// DoctorSummary 是 doctor report 的聚合行。
type DoctorSummary struct {
	Mode          string
	Scenario      string
	Checks        int
	Passed        int
	Reachable     int
	AvgDurationMS float64
	AvgTTFBMS     float64
	FirstAt       time.Time
	LastAt        time.Time
}

// Store SQLite 存储。构造用 Open；Close 等待在途写完成。
type Store struct {
	db *sql.DB

	ch     chan probeRow // 探测日志异步队列
	last   chan lastGoodOp
	doctor chan doctorOp
	wg     sync.WaitGroup
	stop   chan struct{}
}

// schemaExtras 同包分文件注册的追加 DDL（Open 时依序执行，全部幂等）。
var schemaExtras []string

type lastGoodOp struct {
	domain, ip string
	score, rtt float64
}

type doctorOp struct{ r DoctorRecord }

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
CREATE TABLE IF NOT EXISTS sysproxy_snapshot (
  id           INTEGER PRIMARY KEY CHECK (id = 1), -- 单行：接管前原值
  proxy_server TEXT NOT NULL,                      -- W4.5 前遗留列，恒空串
  pac_url      TEXT NOT NULL,                      -- W4.5 前遗留列，恒空串
  taken_at     INTEGER NOT NULL,
  setting_json TEXT                               -- 完整 Setting JSON（W4.5）
);
CREATE TABLE IF NOT EXISTS doctor_log (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id        TEXT NOT NULL,
  started_at    INTEGER NOT NULL,
  mode          TEXT NOT NULL,
  scenario      TEXT NOT NULL,
  name          TEXT NOT NULL,
  target        TEXT NOT NULL,
  ok            INTEGER NOT NULL,
  reachable     INTEGER NOT NULL,
  status        INTEGER NOT NULL,
  class         TEXT NOT NULL,
  duration_ms   REAL NOT NULL,
  ttfb_ms       REAL NOT NULL,
  bytes         INTEGER NOT NULL,
  rate_bps      REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_probe_ts ON probe_log(ts);
CREATE INDEX IF NOT EXISTS idx_doctor_started ON doctor_log(started_at);
CREATE INDEX IF NOT EXISTS idx_doctor_run ON doctor_log(run_id);`); err != nil {
		db.Close()
		return nil, err
	}
	// W4.5 迁移：老库补 setting_json 列（列已存在时报错忽略）。
	// v0.1 未发布，无真实快照需要迁移；旧行 setting_json=NULL 视为无快照。
	_, _ = db.Exec(`ALTER TABLE sysproxy_snapshot ADD COLUMN setting_json TEXT`)
	// W4 迁移：git/ssh 托管快照（kind = 'git' | 'ssh'；payload 由调用方
	// 序列化——git 存键值 JSON，ssh 存整文件 base64）。
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS managed_snapshot(
kind TEXT PRIMARY KEY,
payload TEXT NOT NULL,
taken_at INTEGER NOT NULL)`)

	// 追加 schema（同包分文件注册；全部幂等 IF NOT EXISTS）。
	for _, ddl := range schemaExtras {
		if _, err := db.Exec(ddl); err != nil {
			db.Close()
			return nil, err
		}
	}

	s := &Store{
		db:     db,
		ch:     make(chan probeRow, 1024),
		last:   make(chan lastGoodOp, 64),
		doctor: make(chan doctorOp, 1024),
		stop:   make(chan struct{}),
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
			case d := <-s.doctor:
				s.execDoctor(d.r)
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
		case d := <-s.doctor:
			s.execDoctor(d.r)
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

func (s *Store) execDoctor(r DoctorRecord) {
	_, _ = s.db.Exec(`INSERT INTO doctor_log(
run_id, started_at, mode, scenario, name, target, ok, reachable, status,
class, duration_ms, ttfb_ms, bytes, rate_bps)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.RunID, r.StartedAt.UnixMilli(), r.Mode, r.Scenario, r.Name, r.Target,
		b2i(r.OK), b2i(r.Reachable), r.Status, r.Class, r.DurationMS, r.TTFBMS,
		r.Bytes, r.RateBPS)
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

// AppendDoctor 异步追加一次 doctor 子检查。队列满时丢弃，不能阻塞
// 正常业务路径；doctor report 的 run_id 允许调用者整组关联。
func (s *Store) AppendDoctor(r DoctorRecord) {
	select {
	case s.doctor <- doctorOp{r: r}:
	default:
	}
}

// DoctorSummary 返回 since 之后按 mode/scenario 聚合的探针结果。
func (s *Store) DoctorSummary(since time.Time) ([]DoctorSummary, error) {
	rows, err := s.db.Query(`SELECT mode, scenario, COUNT(*), SUM(ok), SUM(reachable),
COALESCE(AVG(duration_ms),0), COALESCE(AVG(ttfb_ms),0), MIN(started_at), MAX(started_at)
FROM doctor_log WHERE started_at >= ? GROUP BY mode, scenario ORDER BY mode, scenario`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DoctorSummary
	for rows.Next() {
		var x DoctorSummary
		var first, last int64
		if err := rows.Scan(&x.Mode, &x.Scenario, &x.Checks, &x.Passed, &x.Reachable,
			&x.AvgDurationMS, &x.AvgTTFBMS, &first, &last); err != nil {
			return nil, err
		}
		x.FirstAt, x.LastAt = time.UnixMilli(first), time.UnixMilli(last)
		out = append(out, x)
	}
	return out, rows.Err()
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

// --- 快照（sysproxy 接管前的原值，单行表；崩溃对账用） ---

// SaveSnapshot 持久化接管前原值（同步，低频操作）。W4.5 起存完整
// Setting 的 JSON（含 ProxyEnabled/ProxyOverride/AutoDetect）；
// 存储层不 import sysproxy，序列化由调用方完成。
func (s *Store) SaveSnapshotJSON(settingJSON string) error {
	_, err := s.db.Exec(`INSERT INTO sysproxy_snapshot(id, proxy_server, pac_url, taken_at, setting_json)
VALUES(1, '', '', ?, ?)
ON CONFLICT(id) DO UPDATE SET taken_at=excluded.taken_at, setting_json=excluded.setting_json`,
		time.Now().UnixMilli(), settingJSON)
	return err
}

// LoadTakeoverState 接管态查询（W3a）：on = 快照行存在——与
// ensureReconcile 崩溃对账同源判定，零新语义。takenAt 仅在 on=true 有效。
func (s *Store) LoadTakeoverState() (on bool, takenAt time.Time, err error) {
	var ms int64
	err = s.db.QueryRow(`SELECT taken_at FROM sysproxy_snapshot WHERE id=1`).Scan(&ms)
	if err == sql.ErrNoRows {
		return false, time.Time{}, nil
	}
	if err != nil {
		return false, time.Time{}, err
	}
	return true, time.UnixMilli(ms), nil
}

// LoadSnapshotJSON 读快照 JSON；不存在或为 W4.5 之前的旧行返回 ok=false。
func (s *Store) LoadSnapshotJSON() (settingJSON string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT setting_json FROM sysproxy_snapshot WHERE id=1`).
		Scan(&settingJSON)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if settingJSON == "" {
		return "", false, nil // 旧行（升级前遗留），视为无快照
	}
	return settingJSON, true, nil
}

// DeleteSnapshot 删除快照（恢复完成后调用）。
func (s *Store) DeleteSnapshot() error {
	_, err := s.db.Exec(`DELETE FROM sysproxy_snapshot WHERE id=1`)
	return err
}

// --- 托管快照（git/ssh 集成；同步低频，同 sysproxy 快照三函数模式） ---

// SaveManagedSnapshot 按种类持久化托管快照。payload 由调用方序列化。
func (s *Store) SaveManagedSnapshot(kind, payload string) error {
	_, err := s.db.Exec(`INSERT INTO managed_snapshot(kind, payload, taken_at) VALUES(?, ?, ?)
ON CONFLICT(kind) DO UPDATE SET payload=excluded.payload, taken_at=excluded.taken_at`,
		kind, payload, time.Now().UnixMilli())
	return err
}

// LoadManagedSnapshot 读托管快照；不存在返回 ok=false。
func (s *Store) LoadManagedSnapshot(kind string) (payload string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT payload FROM managed_snapshot WHERE kind=?`, kind).Scan(&payload)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return payload, true, nil
}

// DeleteManagedSnapshot 删除托管快照（恢复完成后调用）。
func (s *Store) DeleteManagedSnapshot(kind string) error {
	_, err := s.db.Exec(`DELETE FROM managed_snapshot WHERE kind=?`, kind)
	return err
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
