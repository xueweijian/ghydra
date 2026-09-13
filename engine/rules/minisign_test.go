package rules

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 测试向量：testdata/minisign/ 下 pub.key + v42/v43 为官方 minisign 0.11
// 真实签出（fixture 密钥，ED/prehashed 默认算法，trusted comment 含版本+sha256）。
// 私钥绝不进仓库；签名方向的单向互操作以官方向量证明，
// Go 签名方向由 scripts/sign-rules 产出后经官方 -V 交叉验证（CI rules job）。

const fixtureDir = "testdata/minisign"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func fixturePub(t *testing.T) (pub []byte, keyID []byte) {
	t.Helper()
	p, id, err := ParsePublicKey(readFixture(t, "pub.key"))
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	return p, id
}

// TestOfficialVectors 用官方工具签出的向量验证完整验签路径。
func TestOfficialVectors(t *testing.T) {
	pub, keyID := fixturePub(t)
	for _, v := range []string{"v42", "v43"} {
		data := readFixture(t, v+".json")
		sig := readFixture(t, v+".json.minisig")
		if err := VerifyMinisign(pub, keyID, sig, data); err != nil {
			t.Fatalf("%s: official vector rejected: %v", v, err)
		}
	}
}

// TestOfficialVectorTamper 内容改一字节必拒（A2）。
func TestOfficialVectorTamper(t *testing.T) {
	pub, keyID := fixturePub(t)
	data := readFixture(t, "v42.json")
	sig := readFixture(t, "v42.json.minisig")
	tampered := append([]byte(nil), data...)
	tampered[0] ^= 0x01
	if err := VerifyMinisign(pub, keyID, sig, tampered); err == nil {
		t.Fatal("tampered content accepted")
	}
}

// TestTrustedCommentTamper trusted comment 篡改必拒（global signature 覆盖它）。
func TestTrustedCommentTamper(t *testing.T) {
	pub, keyID := fixturePub(t)
	data := readFixture(t, "v42.json")
	sig := readFixture(t, "v42.json.minisig")
	lines := strings.Split(string(sig), "\n")
	lines[2] = strings.Replace(lines[2], "v42", "v99", 1) // 只动 trusted comment
	tampered := []byte(strings.Join(lines, "\n"))
	if err := VerifyMinisign(pub, keyID, tampered, data); err == nil {
		t.Fatal("tampered trusted comment accepted")
	}
}

// TestUntrustedCommentFree untrusted comment 可任意改动，不影响验证（规范行为）。
func TestUntrustedCommentFree(t *testing.T) {
	pub, keyID := fixturePub(t)
	data := readFixture(t, "v42.json")
	sig := readFixture(t, "v42.json.minisig")
	lines := strings.Split(string(sig), "\n")
	lines[0] = "untrusted comment: totally rewritten by anyone"
	free := []byte(strings.Join(lines, "\n"))
	if err := VerifyMinisign(pub, keyID, free, data); err != nil {
		t.Fatalf("untrusted comment rewrite rejected: %v", err)
	}
}

// TestKeyIDMismatch 错误 key_id 必拒（密钥混用防护）。
func TestKeyIDMismatch(t *testing.T) {
	pub, _ := fixturePub(t)
	data := readFixture(t, "v42.json")
	sig := readFixture(t, "v42.json.minisig")
	if err := VerifyMinisign(pub, []byte{1, 2, 3, 4, 5, 6, 7, 8}, sig, data); err == nil {
		t.Fatal("wrong key_id accepted")
	}
	// 不带期望 key_id 时跳过比对（宽松模式），签名本身仍须有效。
	if err := VerifyMinisign(pub, nil, sig, data); err != nil {
		t.Fatalf("nil keyID strictness leaked: %v", err)
	}
}

// TestMalformedSig 结构非法全拒。
func TestMalformedSig(t *testing.T) {
	if _, err := ParseMinisig([]byte("one line only")); err == nil {
		t.Fatal("1-line accepted")
	}
	if _, err := ParseMinisig([]byte("untrusted comment: x\nbm90IGJhc2U2NA==\ntrusted comment: t\nAAAA")); err == nil {
		t.Fatal("bad base64 accepted")
	}
	// 未知算法字段。
	bad := "untrusted comment: x\n" +
		"YXpEhgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n" +
		"trusted comment: t\nAAAA"
	if _, err := ParseMinisig([]byte(bad)); err == nil {
		t.Fatal("unknown algo accepted")
	}
}

// TestParsePublicKey 形状与 key_id 提取。
func TestParsePublicKey(t *testing.T) {
	_, keyID := fixturePub(t)
	// 注意：该 minisign 实现公钥注释行的 key_id（FA3927CECF572245）是展示名，
	// blob 内真实 key_id（452257cfce2739fa）才与签名 blob 匹配——以 blob 为准
	// （TestOfficialVectors 已证签名匹配此值）。sign-rules 打印指纹必须用 blob 值。
	if got := hex.EncodeToString(keyID); got != "452257cfce2739fa" {
		t.Fatalf("key_id = %s, want 452257cfce2739fa", got)
	}
}
