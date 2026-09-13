package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"crypto/ed25519"

	"github.com/xueweijian/ghydra/engine/rules"
)

// Checksums 文件名 → sha256 hex（小写）。
type Checksums map[string]string

// ParseChecksums 解析 sha256sum 风格清单："<64位hex>  <filename>"（双空格
// 标准，单空格容忍）。拒绝：非 hex、长度≠64、缺文件名、重复文件名、
// 多余字段（一个文件名两个哈希= mix-and-match 面）。
func ParseChecksums(data []byte) (Checksums, error) {
	cs := Checksums{}
	for lineNo, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("selfupdate: checksums 第 %d 行字段数 = %d（须 2）", lineNo+1, len(fields))
		}
		sum := strings.ToLower(fields[0])
		if len(sum) != sha256.Size*2 {
			return nil, fmt.Errorf("selfupdate: checksums 第 %d 行哈希长度 = %d（须 64）", lineNo+1, len(sum))
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return nil, fmt.Errorf("selfupdate: checksums 第 %d 行非 hex: %w", lineNo+1, err)
		}
		name := fields[1]
		if _, dup := cs[name]; dup {
			return nil, fmt.Errorf("selfupdate: checksums 重复文件名 %q", name)
		}
		cs[name] = sum
	}
	if len(cs) == 0 {
		return nil, errors.New("selfupdate: checksums 为空")
	}
	return cs, nil
}

// VerifyArchive U1 第一防线：资产内容 sha256 必须与清单一致。
func VerifyArchive(cs Checksums, name string, data []byte) error {
	want, ok := cs[name]
	if !ok {
		return fmt.Errorf("selfupdate: 清单缺 %s 的哈希", name)
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%w: %s", ErrChecksumMismatch, name)
	}
	return nil
}

// VerifyChecksumsSignature U1 第二防线：checksums.txt 本身须 minisign 签名
// （trusted comment 也在签名内，篡改必拒）。验证器复用 engine/rules——
// W2 已用官方 minisign 工具互操作验证过该实现，不自说自话。
func VerifyChecksumsSignature(pub ed25519.PublicKey, sigText, data []byte) error {
	return rules.VerifyMinisign(pub, nil, sigText, data)
}
