package rules

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// minisign 兼容 detached 签名验证（M3-W2 设计 §2.2）。
//
// 四行标准格式（jedisct1/minisign 规范）：
//
//	untrusted comment: <任意文本，不参与验证>
//	base64( algo[2] ‖ key_id[8] ‖ signature[64] )
//	trusted comment: <被全局签名覆盖的文本>
//	base64( global_signature[64] )   ← ed25519( signature ‖ trusted_comment )
//
// 支持两种算法字段（自描述，按 blob 头分派）：
//   - "Ed"（legacy）：signature = ed25519(data)
//   - "ED"（prehashed，0.11 默认）：signature = ed25519( blake2b-512(data) )
//
// 与官方工具双向互操作是硬要求：testdata/minisign/ 存有官方 minisign 0.11
// 真实签出的向量（CI 另用官方二进制交叉验证 repo 真源，见 ci-rules job）。
// 零第三方签名实现——编解码手写可审计，与 engine/sni 同一工程哲学。

const (
	sigAlgoLegacy  = "Ed" // 非 prehash
	sigAlgoHashed  = "ED" // blake2b-512 prehash（minisign 0.11 默认）
	sigLineComment = "untrusted comment: "
	sigLineTrusted = "trusted comment: "
)

var (
	// ErrSigFormat 签名文件结构非法（行数/base64/长度）。
	ErrSigFormat = errors.New("rules: malformed minisign signature")
	// ErrSigAlgo 未知签名算法字段（既非 Ed 也非 ED）。
	ErrSigAlgo = errors.New("rules: unknown signature algorithm")
	// ErrSigKeyID key_id 与期望公钥不匹配（密钥混用防护）。
	ErrSigKeyID = errors.New("rules: signature key_id mismatch")
	// ErrSigVerify ed25519 验签失败（内容或签名被篡改）。
	ErrSigVerify = errors.New("rules: signature verification failed")
)

// MinisignSig 是解析后的签名结构。
type MinisignSig struct {
	Algo           string
	KeyID          []byte // 8 字节
	Signature      []byte // 64 字节
	TrustedComment string // 冒号后的内容（不含前缀与行尾换行）
	GlobalSig      []byte // 64 字节
}

// ParseMinisig 解析 minisig 四行文本。
func ParseMinisig(text []byte) (*MinisignSig, error) {
	lines := strings.Split(strings.TrimRight(string(text), "\r\n"), "\n")
	if len(lines) != 4 {
		return nil, fmt.Errorf("%w: want 4 lines, got %d", ErrSigFormat, len(lines))
	}
	if !strings.HasPrefix(lines[0], sigLineComment) {
		return nil, fmt.Errorf("%w: line 1 %q", ErrSigFormat, lines[0])
	}
	blob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[1]))
	if err != nil || len(blob) != 2+8+64 {
		return nil, fmt.Errorf("%w: sig blob (%v)", ErrSigFormat, err)
	}
	if !strings.HasPrefix(lines[2], sigLineTrusted) {
		return nil, fmt.Errorf("%w: line 3 %q", ErrSigFormat, lines[2])
	}
	glob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[3]))
	if err != nil || len(glob) != 64 {
		return nil, fmt.Errorf("%w: global signature (%v)", ErrSigFormat, err)
	}
	algo := string(blob[:2])
	if algo != sigAlgoLegacy && algo != sigAlgoHashed {
		return nil, fmt.Errorf("%w: %q", ErrSigAlgo, algo)
	}
	return &MinisignSig{
		Algo:           algo,
		KeyID:          blob[2:10],
		Signature:      blob[10:],
		TrustedComment: strings.TrimPrefix(lines[2], sigLineTrusted),
		GlobalSig:      glob,
	}, nil
}

// VerifyMinisign 验证 minisign 签名。wantKeyID 非空时强制比对（密钥混用防护）。
// 返回 ErrSigVerify 前会先完成 global signature 校验，错误包装原因链完整。
func VerifyMinisign(pub ed25519.PublicKey, wantKeyID, sigText, data []byte) error {
	s, err := ParseMinisig(sigText)
	if err != nil {
		return err
	}
	if len(wantKeyID) == 8 && !bytes.Equal(s.KeyID, wantKeyID) {
		return fmt.Errorf("%w: got %x want %x", ErrSigKeyID, s.KeyID, wantKeyID)
	}
	// 主签名：按算法字段分派消息构造。
	var msg []byte
	if s.Algo == sigAlgoHashed {
		h := blake2b.Sum512(data)
		msg = h[:]
	} else {
		msg = data
	}
	if !ed25519.Verify(pub, msg, s.Signature) {
		return fmt.Errorf("%w: primary signature", ErrSigVerify)
	}
	// 全局签名：ed25519(sig ‖ trusted_comment)，trusted comment 篡改必拒。
	if !ed25519.Verify(pub, append(append([]byte{}, s.Signature...), s.TrustedComment...), s.GlobalSig) {
		return fmt.Errorf("%w: global signature (trusted comment)", ErrSigVerify)
	}
	return nil
}

// ParsePublicKey 解析 minisign 公钥文件（两行）：
// base64( "Ed" ‖ key_id[8] ‖ public_key[32] )，返回 32 字节公钥与 8 字节 key_id。
func ParsePublicKey(text []byte) (pub ed25519.PublicKey, keyID []byte, err error) {
	lines := strings.Split(strings.TrimRight(string(text), "\r\n"), "\n")
	if len(lines) < 2 {
		return nil, nil, fmt.Errorf("%w: public key file", ErrSigFormat)
	}
	blob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[1]))
	if err != nil || len(blob) != 2+8+32 || string(blob[:2]) != sigAlgoLegacy {
		return nil, nil, fmt.Errorf("%w: public key blob (%v)", ErrSigFormat, err)
	}
	return ed25519.PublicKey(blob[10:]), blob[2:10], nil
}
