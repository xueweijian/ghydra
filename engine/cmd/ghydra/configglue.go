package main

// configglue.go —— P4/D6：serve 启动 flags 合并（显式 CLI > 持久化 > 默认）
// 与持久化 key 白名单/语义校验。纯函数（可测）+ 装配胶水分离。
//
// 防御原则：持久化值只在「写入路径」做严格校验（API/CLI 层）；启动合并
// 对已落盘的值再做一次**保守校验**——损坏值（DB 手改/版本演进）忽略回
// 默认并 log，绝不 fail 启动（规则服务可用性优先）。

import (
	"log"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xueweijian/ghydra/engine/store"
)

// configKeys 持久化白名单（POST /api/config 与 ghydra config set 共用）。
var configKeys = map[string]bool{
	"cdn": true, "rules_url": true, "rules_interval": true,
	"listen": true, "doctor_interval": true,
}

// managedDoctorInterval ghydra on 拉起的 serve 默认 doctor 周期。
// 原 spawnServe 硬编码 --doctor-interval 1h 的行为迁移至此（托管模式
// 语义属 serve 自身，不该经命令行伪装成用户显式 flag——否则持久化值
// 永远被压制，GUI 配置活不过重启）。
const managedDoctorInterval = time.Hour

// applyPersistedFlags serve 启动合并（TDD：configglue_test.go 矩阵）。
func applyPersistedFlags(
	explicit map[string]bool,
	persisted map[string]string,
	listen, cdn, rulesURL *string,
	rulesInterval, doctorInterval *time.Duration,
	managed bool,
	logf func(string, ...any),
) {
	pick := func(key string) (string, bool) {
		v, ok := persisted[key]
		return v, ok && v != ""
	}

	if !explicit["listen"] {
		if v, ok := pick("listen"); ok && validLoopbackListen(v) {
			*listen = v
		} else if ok {
			logf("[config] 持久化 listen 非法（%q），忽略用默认", v)
		}
	}
	if !explicit["cdn"] {
		if v, ok := pick("cdn"); ok && validCDN(v) {
			*cdn = v
		} else if ok {
			logf("[config] 持久化 cdn 非法（%q），忽略用默认", v)
		}
	}
	if !explicit["rules-url"] {
		if v, ok := pick("rules_url"); ok && validHTTPURL(v) {
			*rulesURL = v
		} else if ok {
			logf("[config] 持久化 rules_url 非法（%q），忽略用默认", v)
		}
	}
	if !explicit["rules-interval"] {
		if v, ok := pick("rules_interval"); ok {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				*rulesInterval = d
			} else {
				logf("[config] 持久化 rules_interval 非法（%q），忽略用默认", v)
			}
		}
	}
	if !explicit["doctor-interval"] {
		if v, ok := persisted["doctor_interval"]; ok && v != "" {
			if d, err := time.ParseDuration(v); err == nil && d >= 0 {
				*doctorInterval = d
			} else {
				logf("[config] 持久化 doctor_interval 非法（%q），忽略", v)
				if managed {
					*doctorInterval = managedDoctorInterval // 坏值=无有效持久化值，managed 兜底
				}
			}
		} else if managed {
			*doctorInterval = managedDoctorInterval
		}
	}
}

// validLoopbackListen listen 校验：host 必须是 127.0.0.1（安全模型不变：
// 控制面/代理面仅 IPv4 回环），端口 1-65535。
func validLoopbackListen(v string) bool {
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return false
	}
	if host != "127.0.0.1" {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

// validCDN CDN 前缀校验：空合法（关 B 通道）；非空必须 http(s):// 且以 /
// 结尾（切道拼接语义——channel 包按前缀直拼路径）。
func validCDN(v string) bool {
	if v == "" {
		return true
	}
	return validHTTPURL(v) && strings.HasSuffix(v, "/")
}

// validHTTPURL http(s) 绝对 URL 校验。
func validHTTPURL(v string) bool {
	u, err := url.Parse(v)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// loadPersistedConfig 打开 store 读全表。返回打开的 store（调用方复用为
// ruleDB/ConfigSet 写路径，避免二次 Open）；store 不可用返回 (空表, nil)
// ——无持久化时全默认，与既有「--db 空 = 不持久化」容忍语义一致。
func loadPersistedConfig(dbPath string) (map[string]string, *store.Store) {
	if dbPath == "" {
		return map[string]string{}, nil
	}
	db, err := store.Open(dbPath)
	if err != nil {
		log.Printf("[config] 状态库不可用（%v）：无持久化 flags", err)
		return map[string]string{}, nil
	}
	vals, err := db.ConfigList()
	if err != nil {
		log.Printf("[config] config 表读取失败（%v）：全默认", err)
		vals = map[string]string{}
	}
	return vals, db
}
