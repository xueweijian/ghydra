// download.go —— 下载器指标入库与汇总（M2-W2）。
//
// 复用 doctor_log 表（Mode="download"），零迁移：Target 列存通道名
// （A/B），每 Segment 一行，RateBPS 为该段实测速率。汇总查询支撑
// PRD 验收口径「Release 中位速度 ≥ 2MB/s」的数据源。
package store

import (
	"strings"
	"time"

	"github.com/xueweijian/ghydra/engine/get"
)

// AppendDownload 把一次下载的分段指标异步写入 doctor_log。
// runID 关联同一次下载的所有段。
func (s *Store) AppendDownload(runID string, res get.Result) {
	started := time.Now().Add(-time.Duration(res.DurMS * float64(time.Millisecond)))
	for _, seg := range res.Segments {
		// OK 语义（W3 细化）：
		//  - Bytes>0 且带 WhyOut = 切道段（有产出，不算失败）
		//  - Bytes==0 且带 WhyOut = 真失败（dial timeout / 错配拦截等）
		//  - WhyOut=="" = 正常完成（含末段）
		ok := seg.WhyOut == "" || seg.Bytes > 0
		s.AppendDoctor(DoctorRecord{
			RunID:      runID,
			StartedAt:  started,
			Mode:       "download",
			Scenario:   "release",
			Name:       shortURL(res.URL),
			Target:     seg.Channel,
			OK:         ok,
			Reachable:  true,
			Status:     200,
			Class:      "download",
			DurationMS: seg.DurMS,
			TTFBMS:     seg.TTFBMS,
			Bytes:      seg.Bytes,
			RateBPS:    seg.RateBPS,
		})
	}
}

// DownloadStat 单通道下载统计。
type DownloadStat struct {
	Channel   string  `json:"channel"`
	Runs      int     `json:"runs"`     // 有效段（速率>0）
	Attempts  int     `json:"attempts"` // 全部段（含失败零字节段）
	Failures  int     `json:"failures"` // 失败段（W3：A 失败段不再不可见）
	MedianBPS float64 `json:"median_bps"`
	MaxBPS    float64 `json:"max_bps"`
	TotalMB   float64 `json:"total_mb"`
}

// DownloadStats 按通道汇总 since 之后的下载速率（SQLite 无 percentile，
// 中位数用 ORDER BY + LIMIT/OFFSET 取中位行）。
func (s *Store) DownloadStats(since time.Time) ([]DownloadStat, error) {
	var out []DownloadStat
	for _, ch := range []string{"A", "B"} {
		var attempts, failures int
		if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN ok=0 THEN 1 ELSE 0 END),0) FROM doctor_log
WHERE started_at >= ? AND mode='download' AND target=?`,
			since.UnixMilli(), ch).Scan(&attempts, &failures); err != nil {
			return nil, err
		}
		if attempts == 0 {
			continue
		}
		st := DownloadStat{Channel: ch, Attempts: attempts, Failures: failures}
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM doctor_log
WHERE started_at >= ? AND mode='download' AND target=? AND rate_bps > 0`,
			since.UnixMilli(), ch).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			st.Runs = n
			if err := s.db.QueryRow(`SELECT rate_bps, bytes FROM doctor_log
WHERE started_at >= ? AND mode='download' AND target=? AND rate_bps > 0
ORDER BY rate_bps LIMIT 1 OFFSET ?`,
				since.UnixMilli(), ch, n/2).Scan(&st.MedianBPS, new(int64)); err != nil {
				return nil, err
			}
			if err := s.db.QueryRow(`SELECT COALESCE(MAX(rate_bps),0) FROM doctor_log
WHERE started_at >= ? AND mode='download' AND target=? AND rate_bps > 0`,
				since.UnixMilli(), ch).Scan(&st.MaxBPS); err != nil {
				return nil, err
			}
		}
		var totalBytes int64
		_ = s.db.QueryRow(`SELECT COALESCE(SUM(bytes),0) FROM doctor_log
WHERE started_at >= ? AND mode='download' AND target=?`, since.UnixMilli(), ch).Scan(&totalBytes)
		st.TotalMB = float64(totalBytes) / (1 << 20)
		out = append(out, st)
	}
	return out, nil
}

// shortURL 压缩 URL 到可读标识：域名后取前 3 段路径。
func shortURL(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	parts := strings.SplitN(u, "/", 2)
	if len(parts) < 2 {
		return parts[0]
	}
	segs := strings.SplitN(parts[1], "/", 4)
	return parts[0] + "/" + strings.Join(segs[:min(3, len(segs))], "/")
}
