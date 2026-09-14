package store

// config_test.go —— P4/D6 L1：config 表 CRUD（TDD：测试先于实现）。
// 覆盖：Set/Get 幂等、updated_at 单调、List/Delete、坏输入、并发安全面
// （SQLite 单写者模型下顺序写即可——装配层无并发写路径）。

import (
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestConfigSetGet(t *testing.T) {
	db := openTestStore(t)

	if _, ok, err := db.ConfigGet("cdn"); err != nil || ok {
		t.Fatalf("初始应不存在: ok=%v err=%v", ok, err)
	}
	if err := db.ConfigSet("cdn", "https://gh-proxy.com/"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, ok, err := db.ConfigGet("cdn")
	if err != nil || !ok || v != "https://gh-proxy.com/" {
		t.Fatalf("Get: v=%q ok=%v err=%v", v, ok, err)
	}

	// 幂等覆盖（UPSERT）
	if err := db.ConfigSet("cdn", "https://mirror.example/"); err != nil {
		t.Fatalf("Set overwrite: %v", err)
	}
	if v, _, _ := db.ConfigGet("cdn"); v != "https://mirror.example/" {
		t.Fatalf("覆盖失败: %q", v)
	}
	if n, err := db.ConfigCount(); err != nil || n != 1 {
		t.Fatalf("应仍 1 行: n=%d err=%v", n, err)
	}
}

func TestConfigUpdatedAtMonotonic(t *testing.T) {
	db := openTestStore(t)
	_ = db.ConfigSet("listen", "127.0.0.1:9801")
	t1, _ := db.ConfigUpdatedAt("listen")
	_ = db.ConfigSet("listen", "127.0.0.1:9900")
	t2, _ := db.ConfigUpdatedAt("listen")
	if t2 < t1 {
		t.Fatalf("updated_at 必须单调不回退: %d -> %d", t1, t2)
	}
}

func TestConfigListDelete(t *testing.T) {
	db := openTestStore(t)
	for k, v := range map[string]string{
		"cdn": "https://a/", "rules_url": "https://r/json", "rules_interval": "6h",
	} {
		if err := db.ConfigSet(k, v); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}
	all, err := db.ConfigList()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 || all["rules_interval"] != "6h" {
		t.Fatalf("List 不符: %v", all)
	}
	if err := db.ConfigDelete("rules_url"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	all, _ = db.ConfigList()
	if len(all) != 2 {
		t.Fatalf("删除后应 2 行: %v", all)
	}
	if _, ok, _ := db.ConfigGet("rules_url"); ok {
		t.Fatal("rules_url 应已不存在")
	}
	// 删除不存在的 key 不报错
	if err := db.ConfigDelete("nope"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}

func TestConfigEmptyKeyRejected(t *testing.T) {
	db := openTestStore(t)
	if err := db.ConfigSet("", "x"); err == nil {
		t.Fatal("空 key 必须拒绝")
	}
	if err := db.ConfigSet("k", ""); err != nil {
		t.Fatalf("空 value 合法（=置空）: %v", err)
	}
}
