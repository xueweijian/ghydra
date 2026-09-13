package api

// token.go —— 本机 API token 生命周期（M3-W1）。
//
// 独立文件 ~/.ghydra/api-token（0600），与 serve.json 解耦：`ghydra serve`
// 裸跑（CI/dev）与 `ghydra on` 拉起两条路径都自然工作。GUI 壳读同一路径
// 注入；浏览器兜底模式用户经 `ghydra token` 取值粘贴。

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultTokenPath token 文件位置（HOME 不可用返回空）。
func DefaultTokenPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".ghydra", "api-token")
}

// GenToken 生成 32 hex 字符随机 token。
func GenToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("api: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// LoadOrCreateToken 读文件；不存在/空则生成并写（0600，目录 0700）。
// 显式注入（--api-token）的调用方不走本函数。
func LoadOrCreateToken(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("token path empty")
	}
	if b, err := os.ReadFile(path); err == nil && len(b) >= 16 {
		return string(b), nil
	}
	tok := GenToken()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return tok, nil
}
