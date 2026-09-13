package rules

import (
	"fmt"
	"os"
	"path/filepath"
)

// 原子落盘（M3-W2 设计 §4）：tmp 写 + fsync + rename。
//
// json 与 minisig 是两个文件，不存在跨两文件的原子事务；崩溃安全由
// **加载侧成对验签**保证（provider：任一缺失或验签失败即整体回退内嵌地板），
// 写侧只需保证单文件原子可见——rename 语义（同目录）在三大平台均原子。

// SavePairAtomic 原子保存 json+sig 对。base 不含扩展名：文件为
// dir/base 与 dir/base.minisig。写入中途崩溃只留 .tmp 残留，不破坏旧版。
func SavePairAtomic(dir, base string, data, sig []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("rules: mkdir: %w", err)
	}
	jsonPath := filepath.Join(dir, base)
	sigPath := jsonPath + ".minisig"
	// 先 sig 后 json：json rename 到位即视为新版本生效，此时 sig 必须已在。
	if err := writeAtomicFile(sigPath, sig); err != nil {
		return err
	}
	if err := writeAtomicFile(jsonPath, data); err != nil {
		return err
	}
	return syncDir(dir)
}

func writeAtomicFile(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("rules: open %s: %w", tmp, err)
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("rules: write %s: %w", tmp, err)
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("rules: fsync %s: %w", tmp, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("rules: close %s: %w", tmp, err)
	}
	if err = renameAtomic(tmp, path); err != nil {
		return fmt.Errorf("rules: rename %s: %w", tmp, err)
	}
	return nil
}

// LoadPair 读取成对文件（原始字节）。任一缺失返回 os.ErrNotExist 包装；
// 内容级校验（schema+验签）由 provider 负责——本层只保证「读到的字节
// 要么是完整旧对、要么是完整新对」。
// Windows：读写并发窗口内读者打开文件可能撞 transient sharing violation，
// 读侧带重试（readfile_windows.go；posix 直通）。
func LoadPair(dir, base string) (data, sig []byte, err error) {
	data, err = readFileRetry(filepath.Join(dir, base))
	if err != nil {
		return nil, nil, err
	}
	sig, err = readFileRetry(filepath.Join(dir, base+".minisig"))
	if err != nil {
		return nil, nil, fmt.Errorf("rules: sig missing for %s: %w", base, err)
	}
	return data, sig, nil
}
