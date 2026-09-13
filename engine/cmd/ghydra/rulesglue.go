package main

// rulesglue.go —— 规则热更新装配辅助（M3-W2）：rules.Snapshot → api DTO。
// Refresh 状态由 phase 3 refresher 填实，当前零值（schema 已 golden 锁定）。

import (
	"time"

	"github.com/xueweijian/ghydra/engine/api"
	"github.com/xueweijian/ghydra/engine/rules"
)

// mapRulesStatus status/SSE status 帧的规则摘要。stale 每次现算
// （随时间漂移，不缓存）。
func mapRulesStatus(p *rules.Provider) api.RulesStatus {
	snap := p.Snapshot()
	return api.RulesStatus{
		Version: snap.Version,
		Source:  string(snap.Source),
		Stale:   snap.StaleNow(time.Now()),
	}
}

// mapRulesSnapshot GET /api/rules 全景快照。
func mapRulesSnapshot(p *rules.Provider) api.RulesSnapshot {
	snap := p.Snapshot()
	return api.RulesSnapshot{
		Version:      snap.Version,
		Source:       string(snap.Source),
		Stale:        snap.StaleNow(time.Now()),
		GeneratedAt:  snap.GeneratedAt.UTC().Format(time.RFC3339),
		ExpiresAt:    snap.ExpiresAt.UTC().Format(time.RFC3339),
		Domains:      snap.Domains,
		CDNEndpoints: snap.CDNEndpoints,
		SeedIPs:      snap.SeedIPs,
		Refresh:      api.RulesRefreshState{}, // phase 3 refresher
	}
}
