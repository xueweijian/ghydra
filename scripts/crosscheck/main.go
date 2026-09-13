// crosscheck —— CI 专用规则签名交叉验证器（防自证）。
//
// 设计 §7-L1#1/§6：互操作验证不能用我们自己的手写验签器自证。本工具用
// aead.dev/minisign（独立第三方实现，restic 等项目使用）验证 repo 真源
// rules/current.json 的签名。与本地「官方 minisign 0.11 二进制」的实测
// 交叉验证（OFFICIAL_CROSSCHECK_OK，2026-09-13）共同构成双向互操作证据。
//
// 用法: go run ./scripts/crosscheck <pubkey-file> <sig-file> <data-file>
// 验证失败以非零退出（CI job 失败）。
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"aead.dev/minisign"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "用法: crosscheck <pubkey> <sig> <data>")
		os.Exit(2)
	}
	pubFile, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "crosscheck:", err)
		os.Exit(2)
	}
	defer pubFile.Close()
	// 公钥文件第二行 = base64 主体（UnmarshalText 原生支持）。
	var pub minisign.PublicKey
	sc := bufio.NewScanner(pubFile)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "untrusted comment:") || line == "" {
			continue
		}
		if err := pub.UnmarshalText([]byte(line)); err != nil {
			fmt.Fprintln(os.Stderr, "crosscheck pubkey:", err)
			os.Exit(2)
		}
		break
	}
	sig, err := os.ReadFile(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "crosscheck:", err)
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[3])
	if err != nil {
		fmt.Fprintln(os.Stderr, "crosscheck:", err)
		os.Exit(2)
	}
	if !minisign.Verify(pub, data, sig) {
		fmt.Fprintln(os.Stderr, "crosscheck: THIRD-PARTY VERIFICATION FAILED")
		os.Exit(1)
	}
	fmt.Printf("crosscheck OK: %s verified by independent minisign implementation\n", os.Args[3])
}
