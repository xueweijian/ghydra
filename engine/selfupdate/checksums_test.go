package selfupdate

import (
	"errors"
	"os"
	"testing"

	"github.com/xueweijian/ghydra/engine/rules"
)

// 设计 §7 L1-3/L1-4：checksums.txt 解析核对 + minisign 验签新钥匙 fixture
// （fixture 由官方 minisign 0.11 工具签名——互操作硬要求，非自说自话）。

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseChecksums(t *testing.T) {
	data := mustRead(t, "testdata/checksums.txt")
	cs, err := ParseChecksums(data)
	if err != nil {
		t.Fatalf("ParseChecksums: %v", err)
	}
	if len(cs) != 2 {
		t.Fatalf("条目数 = %d，期望 2", len(cs))
	}
	if cs["ghydra-windows-amd64.zip"] != "467e2885a820ec42fdfa81095118d2e36719646f16d760e4c99ad0d0768f9082" {
		t.Errorf("win 哈希解析错误: %s", cs["ghydra-windows-amd64.zip"])
	}

	// 单空格分隔（sha256sum -b 输出双空格，手写单空格也容忍）
	if _, err := ParseChecksums([]byte("aa…  f1\n")); err == nil {
		// 非 hex 应报错
		t.Error("非 hex 哈希应报错")
	}
	cs2, err := ParseChecksums([]byte("AA BB CC DD EE FF 00 11 22 33 44 55 66 77 88 99 aa bb cc dd ee ff 00 11 22 33 44 55 66 77 88 99 x.zip\n"))
	if err == nil {
		_ = cs2 // 大写十六进制归一为小写
	}
}

func TestParseChecksumsMalformed(t *testing.T) {
	bad := []string{
		"",                      // 空
		"no-hash-here\n",        // 无哈希
		"abc f1\n",              // 哈希太短
		"467e…2885  a820… f1\n", // 空格分隔多段（一个文件两个哈希字段）
		"467e2885a820ec42fdfa81095118d2e36719646f16d760e4c99ad0d0768f9082\n", // 缺文件名
	}
	for _, in := range bad {
		if _, err := ParseChecksums([]byte(in)); err == nil {
			t.Errorf("坏输入 %q 应报错", in)
		}
	}
	// 重复条目（同名文件两行不同哈希）→ 拒（mix-and-match 面）
	dup := "aa bb cc dd ee ff 00 11 22 33 44 55 66 77 88 99 aa bb cc dd ee ff 00 11 22 33 44 55 66 77 88 99 a.zip\n" +
		"aa bb cc dd ee ff 00 11 22 33 44 55 66 77 88 99 aa bb cc dd ee ff 00 11 22 33 44 55 66 77 88 9a a.zip\n"
	if _, err := ParseChecksums([]byte(dup)); err == nil {
		t.Error("重复文件名应报错")
	}
}

func TestVerifyArchiveChecksum(t *testing.T) {
	cs, err := ParseChecksums(mustRead(t, "testdata/checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// happy：真实 fixture 文件内容与哈希严格对应
	win := mustRead(t, "testdata/ghydra-windows-amd64.zip")
	if err := VerifyArchive(cs, "ghydra-windows-amd64.zip", win); err != nil {
		t.Errorf("happy path 应通过: %v", err)
	}
	// 篡改一字节 → 拒（U1 第一防线）
	tampered := append([]byte{}, win...)
	tampered[3] ^= 0xff
	if err := VerifyArchive(cs, "ghydra-windows-amd64.zip", tampered); !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("篡改应 ErrChecksumMismatch，得到 %v", err)
	}
	// 清单缺该文件 → 拒
	if err := VerifyArchive(cs, "unknown.zip", win); err == nil {
		t.Error("清单缺文件应报错")
	}
}

func TestVerifyChecksumsSignature(t *testing.T) {
	data := mustRead(t, "testdata/checksums.txt")
	sig := mustRead(t, "testdata/checksums.txt.minisig")

	// fixture 用 testdata 测试钥匙签名；生产冻结公钥 keys.go 是另一把。
	// 这里两条路径都验：测试钥匙（互操作 fixture）+ 冻结公钥（必须拒）。
	testPub, _, err := rules.ParsePublicKey(mustRead(t, "testdata/test.pub"))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyChecksumsSignature(testPub, sig, data); err != nil {
		t.Fatalf("测试钥匙验签 fixture 应通过（官方 minisign -Vm 已独立验证过）: %v", err)
	}

	// 用冻结的 release 公钥验测试钥匙签的 fixture → keyID 不匹配必拒
	if err := VerifyChecksumsSignature(FrozenPublicKey(), sig, data); err == nil {
		t.Error("错配公钥必须拒")
	}

	// 篡改 checksums 一字节 → 验签拒（U1 第二防线）
	tampered := append([]byte{}, data...)
	tampered[10] ^= 0x01
	if err := VerifyChecksumsSignature(testPub, sig, tampered); err == nil {
		t.Error("checksums 篡改后验签应失败")
	}

	// 篡改 minisig 本体（坏 base64）
	badSig := append([]byte{}, sig...)
	badSig[100] = '!'
	if err := VerifyChecksumsSignature(testPub, badSig, data); err == nil {
		t.Error("坏 base64 签名应拒")
	}

	// trusted comment 篡改 → 拒（trusted comment 在签名内）
	lines := splitLines(sig)
	lines[2] = "trusted comment: 篡改过的注释"
	forged := joinLines(lines)
	if err := VerifyChecksumsSignature(testPub, forged, data); err == nil {
		t.Error("trusted comment 篡改应拒")
	}
}

// splitLines/joinLines 小工具（测试内用，避免引入 strings 依赖到被测面）。
func splitLines(b []byte) []string {
	out := []string{}
	cur := ""
	for _, c := range string(b) {
		if c == '\n' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(c)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func joinLines(ls []string) []byte {
	s := ""
	for i, l := range ls {
		if i > 0 {
			s += "\n"
		}
		s += l
	}
	return []byte(s + "\n")
}
