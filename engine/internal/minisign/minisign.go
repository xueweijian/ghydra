// Package minisign minisign -W 私钥解析与 detached 签名（W4）。
// 生产验签在 engine/rules（W2 已官方工具互操作验证）；签名侧归本包，
// 供 scripts/sign-release（离线发布签名）与测试动态生成 fixture。
package minisign

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// SecretKey -W 无密码私钥 + 其 key_id。
type SecretKey struct {
	Priv  ed25519.PrivateKey
	KeyID []byte // 8 字节
}

// ParseSecretKeyFile 解析 minisign 0.11 -W（无密码）私钥文件。
//
// ⚠️ 布局以实测为准（spec 页面已过时）：base64 后 158 字节：
//
//	"Ed"[2] ‖ kdf_algo[2] ‖ "B2"[2] ‖ salt[32] ‖ opslimit[8] ‖
//	memlimit[8] ‖ key_id[8] ‖ seed(32)‖pk(32) ‖ cksum[32]
//
// -W 模式 salt/ops/mem/cksum 全零；seed 展开公钥必须与 pk 字段一致
// （配对完整性校验）。加密型私钥（kdf 非零）直接拒。
func ParseSecretKeyFile(path string) (SecretKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return SecretKey{}, err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\r\n"), "\n")
	if len(lines) < 2 {
		return SecretKey{}, errors.New("私钥文件格式非法（少于两行）")
	}
	blob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[1]))
	if err != nil {
		return SecretKey{}, fmt.Errorf("私钥 base64 非法: %w", err)
	}
	if len(blob) != 158 {
		return SecretKey{}, fmt.Errorf("私钥 blob %d 字节，仅支持 -W 无密码格式（158 字节）", len(blob))
	}
	if string(blob[0:2]) != "Ed" {
		return SecretKey{}, fmt.Errorf("私钥算法字段 %q 非 Ed", blob[0:2])
	}
	if blob[2] != 0 || blob[3] != 0 {
		return SecretKey{}, errors.New("kdf 段非空：疑似加密型私钥，不支持")
	}
	if string(blob[4:6]) != "B2" {
		return SecretKey{}, errors.New("checksum 算法字段非 blake2b")
	}
	sk := blob[62:126]
	priv := ed25519.PrivateKey(append([]byte{}, sk...))
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok || !equal(sk[32:], pub) {
		return SecretKey{}, errors.New("私钥内部 seed/pk 不自洽（文件损坏）")
	}
	return SecretKey{Priv: priv, KeyID: blob[54:62]}, nil
}

// SignDetached 产出 minisign 0.11 默认算法（"ED" prehashed，blake2b-512）
// 的四行签名文本——官方 `minisign -Vm <file> -P <公钥>` 可独立验证。
func SignDetached(sk SecretKey, data []byte, untrustedComment, trustedComment string) ([]byte, error) {
	h := blake2b.Sum512(data)
	msg := h[:]
	sig := ed25519.Sign(sk.Priv, msg)
	pub, _ := sk.Priv.Public().(ed25519.PublicKey)
	if !ed25519.Verify(pub, msg, sig) {
		return nil, errors.New("minisign: self-verify failed — 密钥材料损坏")
	}
	global := ed25519.Sign(sk.Priv, append(append([]byte{}, sig...), []byte(trustedComment)...))
	blob := append(append([]byte("ED"), sk.KeyID...), sig...)
	var b strings.Builder
	fmt.Fprintf(&b, "untrusted comment: %s\n", untrustedComment)
	b.WriteString(base64.StdEncoding.EncodeToString(blob))
	b.WriteByte('\n')
	fmt.Fprintf(&b, "trusted comment: %s\n", trustedComment)
	b.WriteString(base64.StdEncoding.EncodeToString(global))
	b.WriteByte('\n')
	return []byte(b.String()), nil
}

func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
