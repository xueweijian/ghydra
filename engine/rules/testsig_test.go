package rules

import (
	"crypto/ed25519"
	"encoding/base64"
)

// 测试专用：真实 ed25519 密钥 + minisign 四行格式（"Ed" 非 prehash）。
// 本文件只服务原子读写测试的「可验签对」构造；密码学正确性/互操作性
// 由 minisign_test.go 用官方 minisign 0.11 真实向量覆盖。

// testSigner 一次性测试密钥的签名构造器。
type testSigner struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func newTestSigner() *testSigner {
	pub, priv, _ := ed25519.GenerateKey(nil)
	return &testSigner{priv: priv, pub: pub}
}

// signFile 产出 data 的完整 minisig 四行文本（trusted comment 被全局签名覆盖）。
func (s *testSigner) signFile(data []byte, trusted string) []byte {
	sig := ed25519.Sign(s.priv, data) // "Ed"：直接签内容
	blob := append([]byte("Ed"), make([]byte, 8)...)
	blob = append(blob, sig...)
	global := ed25519.Sign(s.priv, append(append([]byte{}, sig...), trusted...))
	return []byte("untrusted comment: test\n" +
		base64.StdEncoding.EncodeToString(blob) + "\n" +
		"trusted comment: " + trusted + "\n" +
		base64.StdEncoding.EncodeToString(global) + "\n")
}

// verifyFile 与 provider 加载行为同构：VerifyMinisign 全路径。
func (s *testSigner) verifyFile(data, sigText []byte) error {
	return VerifyMinisign(s.pub, nil, sigText, data)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
