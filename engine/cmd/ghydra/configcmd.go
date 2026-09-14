package main

// configcmd.go —— P4/D6：`ghydra config set|get|list`。
//
// 直接写持久化 config 表（serve 启动合并逻辑读取；daemon 在跑时写入
// 亦安全——SQLite 单写者 + 低频写；生效需重启 daemon，输出明确提示）。
// key 白名单与值语义校验与 POST /api/config 同源（configKeys + 校验函数）。

import (
	"fmt"
	"os"
	"time"

	"github.com/xueweijian/ghydra/engine/store"
)

func configCmd(args []string) {
	if len(args) == 0 {
		configUsage()
		os.Exit(2)
	}
	dbPath := defaultDBPath()
	sub, rest := args[0], args[1:]
	switch sub {
	case "set":
		if len(rest) != 2 {
			fmt.Fprintln(os.Stderr, "用法: ghydra config set <key> <value>")
			os.Exit(2)
		}
		configSet(dbPath, rest[0], rest[1])
	case "get":
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, "用法: ghydra config get <key>")
			os.Exit(2)
		}
		configGet(dbPath, rest[0])
	case "list":
		configList(dbPath)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: config %s\n", sub)
		configUsage()
		os.Exit(2)
	}
}

func configUsage() {
	fmt.Fprint(os.Stderr, `用法:
  ghydra config set <key> <value>   写持久化配置（key: cdn | rules_url | rules_interval | listen | doctor_interval）
  ghydra config get <key>           读单值
  ghydra config list                列出全部持久化值
生效时机: serve 启动时合并（显式 flag > 持久化值 > 默认）；daemon 在跑需重启生效。
`)
}

// configValidate 写入前的值语义校验（与 POST /api/config 同源规则）。
func configValidate(key, value string) error {
	if !configKeys[key] {
		return fmt.Errorf("未知 key %q（合法: cdn rules_url rules_interval listen doctor_interval）", key)
	}
	switch key {
	case "listen":
		if !validLoopbackListen(value) {
			return fmt.Errorf("listen 必须形如 127.0.0.1:端口（仅 IPv4 回环）")
		}
	case "cdn":
		if !validCDN(value) {
			return fmt.Errorf("cdn 必须是以 / 结尾的 http(s) URL（空 = 关 B 通道）")
		}
	case "rules_url":
		if value != "" && !validHTTPURL(value) {
			return fmt.Errorf("rules_url 必须是 http(s) 绝对 URL（空 = 官方源）")
		}
	case "rules_interval", "doctor_interval":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("%s 必须是 Go duration 串（如 6h / 30m）", key)
		}
		if key == "rules_interval" && d <= 0 {
			return fmt.Errorf("rules_interval 必须为正")
		}
		if key == "doctor_interval" && d < 0 {
			return fmt.Errorf("doctor_interval 不能为负")
		}
	}
	return nil
}

func configSet(dbPath, key, value string) {
	if err := configValidate(key, value); err != nil {
		fmt.Fprintf(os.Stderr, "拒绝: %v\n", err)
		os.Exit(2)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "状态库不可用: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := db.ConfigSet(key, value); err != nil {
		fmt.Fprintf(os.Stderr, "写入失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("已持久化 %s = %s（serve 启动时生效；daemon 在跑需 ghydra off && ghydra on）\n", key, value)
}

func configGet(dbPath, key string) {
	if !configKeys[key] {
		fmt.Fprintf(os.Stderr, "未知 key %q\n", key)
		os.Exit(2)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "状态库不可用: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	v, ok, err := db.ConfigGet(key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		os.Exit(1)
	}
	if !ok {
		fmt.Println("(未持久化，用默认值)")
		return
	}
	fmt.Println(v)
}

func configList(dbPath string) {
	db, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "状态库不可用: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	vals, err := db.ConfigList()
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		os.Exit(1)
	}
	if len(vals) == 0 {
		fmt.Println("(空——全部用默认值)")
		return
	}
	for _, k := range []string{"listen", "cdn", "rules_url", "rules_interval", "doctor_interval"} {
		if v, ok := vals[k]; ok {
			fmt.Printf("%s = %s\n", k, v)
		}
	}
}
