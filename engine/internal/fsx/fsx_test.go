package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomicRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := WriteFileAtomic(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 覆盖写（open 已存在文件不降权限）
	if err := WriteFileAtomic(path, []byte("v2-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFileRetry(path)
	if err != nil || string(got) != "v2-longer" {
		t.Fatalf("got %q err %v", got, err)
	}
	// 无 tmp 残留
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Errorf("残留文件: %v", entries)
	}
	// 权限
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("perm = %v", fi.Mode().Perm())
	}
}

func TestWriteFileAtomicNewDir(t *testing.T) {
	// 父目录不存在 → 报错清晰（调用方负责 MkdirAll；不隐式创建）
	path := filepath.Join(t.TempDir(), "sub", "f")
	if err := WriteFileAtomic(path, []byte("x"), 0o644); err == nil {
		t.Error("父目录缺失应报错")
	}
}

func TestRenameAtomicReplaceExisting(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst")
	src := filepath.Join(dir, "src")
	os.WriteFile(dst, []byte("old"), 0o644)
	os.WriteFile(src, []byte("new"), 0o644)
	if err := RenameAtomic(src, dst); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "new" {
		t.Errorf("dst = %q", got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("src 应已不存在")
	}
}
