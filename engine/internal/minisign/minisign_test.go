package minisign

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// 互操作硬要求：本包签出的签名必须能被官方 minisign 工具验证。
// 沙箱装有 minisign 0.11；无该工具的环境（CI 无）跳过——互操作主证
// 在 rules 包官方向量测试（W2）+ 本地 crosscheck 脚本双保险。

func TestSignDetachedRoundtripWithOfficialTool(t *testing.T) {
	if _, err := exec.LookPath("minisign"); err != nil {
		t.Skip("无官方 minisign 工具（CI 跳过，本地必跑）")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "k")
	pub := filepath.Join(dir, "k.pub")
	if out, err := exec.Command("minisign", "-W", "-G", "-s", key, "-p", pub).CombinedOutput(); err != nil {
		t.Fatalf("生成密钥: %v\n%s", err, out)
	}
	sk, err := ParseSecretKeyFile(key)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("selfupdate fixture: 这不是规则签名，是 release checksums 签名\nline2")
	sig, err := SignDetached(sk, msg, "untrusted comment: ghydra release", "ghydra release v1.2.3 test")
	if err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(dir, "msg.bin")
	sigPath := filepath.Join(dir, "msg.minisig")
	os.WriteFile(msgPath, msg, 0o644)
	os.WriteFile(sigPath, sig, 0o644)
	// 官方工具独立验证（-P 公钥串从 pub 文件取）
	pubText, _ := os.ReadFile(pub)
	pubLine := ""
	for _, l := range splitLn(string(pubText)) {
		if l != "" && !startsWith(l, "untrusted") {
			pubLine = l
		}
	}
	if out, err := exec.Command("minisign", "-Vm", msgPath, "-x", sigPath, "-P", pubLine, "-Q").CombinedOutput(); err != nil {
		t.Fatalf("官方 minisign 验证失败: %v\n%s\nsig:\n%s", err, out, sig)
	}
}

func splitLn(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
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

func startsWith(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}
