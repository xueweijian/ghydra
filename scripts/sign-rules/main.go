// sign-rules —— GHydra 规则签名工具（M3-W2 设计 §6）。
//
// 用法：
//
//	go run ./scripts/sign-rules -key <minisign.key> -in rules/current.json
//	go run ./scripts/sign-rules -key ... -in ... -force   # 仅限密钥轮换
//
// 职责：①签名前先过与运行时完全相同的 schema 校验器（坏文件签不出去）；
// ②版本单调防手滑倒退（rules/.lastsigned 本地状态，gitignore）；
// ③产出 minisign 0.11 默认算法（"ED"，blake2b-512 prehash）的四行签名——
// 官方 `minisign -Vm rules/current.json -P <公钥>` 可独立验证（互操作硬要求）。
//
// 私钥永不进 repo/CI：本地离线签 + CI 只验不签（拍板点 #1）。
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/blake2b"

	"github.com/xueweijian/ghydra/engine/rules"
)

func main() {
	key := flag.String("key", "", "minisign 私钥文件（-W 无密码格式）")
	in := flag.String("in", "", "rules json 文件")
	check := flag.Bool("check", false, "仅校验 schema 与签名存在性（CI 模式，无需私钥）")
	force := flag.Bool("force", false, "跳过版本单调检查（仅限密钥轮换/重置）")
	unsafe := flag.Bool("unsafe", false, "跳过 schema 校验（仅限测试向量生成——签出的文件运行时会拒，毒化矩阵 fixture 用）")
	flag.Parse()
	if *in == "" || (*key == "" && !*check) {
		fmt.Fprintln(os.Stderr, "用法: sign-rules -key <seckey> -in <rules.json> [-force] | -check -in <rules.json>")
		os.Exit(2)
	}
	if *check {
		if err := checkOnly(*in); err != nil {
			fmt.Fprintln(os.Stderr, "sign-rules -check:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(*key, *in, *force, *unsafe); err != nil {
		fmt.Fprintln(os.Stderr, "sign-rules:", err)
		os.Exit(1)
	}
}

// checkOnly CI 模式：schema 与运行时同校验器 + 签名文件存在 + trusted comment
// 的 sha256 与内容一致（签出的文件未被事后改动到内容/签名脱钩）。
func checkOnly(inPath string) error {
	data, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	rf, err := rules.ParseRules(data)
	if err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	sigPath := inPath + ".minisig"
	sigText, err := os.ReadFile(sigPath)
	if err != nil {
		return fmt.Errorf("签名文件缺失: %w", err)
	}
	s, err := rules.ParseMinisig(sigText)
	if err != nil {
		return fmt.Errorf("签名格式: %w", err)
	}
	sum := sha256.Sum256(data)
	want := fmt.Sprintf("sha256=%x", sum)
	if !strings.Contains(s.TrustedComment, want) {
		return fmt.Errorf("trusted comment 指纹与内容不符（want %s）", want)
	}
	fmt.Printf("check OK: version=%d domains=%d cdn=%d\n", rf.Version, len(rf.Domains), len(rf.CDNEndpoints))
	return nil
}

func run(keyPath, inPath string, force, unsafe bool) error {
	data, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	// 与运行时同一解析器：签名前确保能被所有客户端接受。
	rf, err := rules.ParseRules(data)
	version := int64(0)
	if err != nil {
		if !unsafe {
			return fmt.Errorf("schema（与运行时同校验器）: %w", err)
		}
		fmt.Fprintln(os.Stderr, "⚠️  -unsafe：schema 校验跳过（测试向量，运行时必拒）")
	} else {
		version = rf.Version
	}
	last, err := readLastSigned(filepath.Dir(inPath))
	if err != nil {
		return err
	}
	if !unsafe && !force && version <= last {
		return fmt.Errorf("version %d <= 上次已签 %d；如为密钥轮换/重置请 -force", rf.Version, last)
	}
	priv, keyID, err := parseSecretKeyFile(keyPath)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	sig := signAndSelfVerify(priv, digestPrehash(data))
	trusted := fmt.Sprintf("ghydra-rules v%d sha256=%x", version, sum)
	global := ed25519.Sign(priv, append(append([]byte{}, sig...), trusted...))
	blob := append([]byte("ED"), keyID...)
	blob = append(blob, sig...)

	out := inPath + ".minisig"
	text := "untrusted comment: ghydra rules\n" +
		base64.StdEncoding.EncodeToString(blob) + "\n" +
		"trusted comment: " + trusted + "\n" +
		base64.StdEncoding.EncodeToString(global) + "\n"
	if err := os.WriteFile(out, []byte(text), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(inPath), ".lastsigned"),
		[]byte(fmt.Sprintf("%d\n", version)), 0o644); err != nil {
		return err
	}
	fmt.Printf("signed %-24s version=%d key_id=%s sha256=%x\n",
		filepath.Base(out), version, hex(keyID), sum)
	return nil
}

// digestPrehash minisign "ED" 算法的消息构造：blake2b-512(data)。
func digestPrehash(data []byte) []byte {
	h := blake2b.Sum512(data)
	return h[:]
}

// parseSecretKeyFile 解析 minisign -W（无密码）私钥文件。
//
// ⚠️ 布局以实测为准（spec 页面已过时）：minisign 0.11 -W 模式实际产出 158 字节：
//
//	base64( "Ed"[2] ‖ kdf_algo[2] ‖ "B2"[2] ‖ salt[32] ‖ opslimit[8] ‖
//	        memlimit[8] ‖ key_id[8] ‖ seed[32] ‖ pk[32] ‖ cksum[32] )  共 158
//
// -W 模式 salt/ops/mem/cksum 全零（cksum 仅加密模式有意义）；seed 展开
// 公钥必须与 pk 字段一致（完整性配对校验：坏文件在此被拒，漏网者签出的
// 签名在运行时验签必拒，防线后移但闭合）。加密型私钥（kdf 非零）直接拒。
func parseSecretKeyFile(path string) (ed25519.PrivateKey, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\r\n"), "\n")
	if len(lines) < 2 {
		return nil, nil, errors.New("私钥文件格式非法（少于两行）")
	}
	blob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[1]))
	if err != nil {
		return nil, nil, fmt.Errorf("私钥 base64 非法: %v", err)
	}
	if len(blob) != 158 {
		return nil, nil, fmt.Errorf("私钥 blob %d 字节，仅支持 -W 无密码格式（158 字节）", len(blob))
	}
	if string(blob[0:2]) != "Ed" {
		return nil, nil, fmt.Errorf("私钥算法字段 %q 非 Ed", blob[0:2])
	}
	if blob[2] != 0 || blob[3] != 0 {
		return nil, nil, errors.New("kdf 段非空：疑似加密型私钥，本工具不支持")
	}
	if string(blob[4:6]) != "B2" {
		return nil, nil, errors.New("checksum 算法字段非 blake2b")
	}
	keyID := blob[54:62]
	sk := blob[62:126] // libsodium secretkey = seed(32) ‖ pk(32)
	// 126..158 = cksum[32]，-W 模式全零（仅加密模式有意义）。
	// 本地无 checksum 可验（-W 布局如此）；完整性由「签出的签名必须能被
	// 公钥验回」在链路末端兜底（sign-rules 输出前自验一次，见下）。
	priv := ed25519.PrivateKey(append([]byte{}, sk...))
	if !equal(sk[32:], priv.Public().(ed25519.PublicKey)) {
		return nil, nil, errors.New("私钥内部 seed/pk 不自洽（文件损坏）")
	}
	return priv, keyID, nil
}

// signAndSelfVerify 签名后立即用解析出的私钥派生公钥自验，防坏钥匙出坏签名。
func signAndSelfVerify(priv ed25519.PrivateKey, msg []byte) []byte {
	sig := ed25519.Sign(priv, msg)
	if !ed25519.Verify(priv.Public().(ed25519.PublicKey), msg, sig) {
		panic("sign-rules: self-verify failed — key material corrupted")
	}
	return sig
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

func readLastSigned(dir string) (int64, error) {
	b, err := os.ReadFile(filepath.Join(dir, ".lastsigned"))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var v int64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &v); err != nil {
		return 0, fmt.Errorf(".lastsigned 非法: %w", err)
	}
	return v, nil
}

func hex(b []byte) string {
	const d = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, d[c>>4], d[c&15])
	}
	return string(out)
}
