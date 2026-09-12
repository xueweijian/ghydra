// Package sni 实现最小、可审计的 TLS ClientHello 解析与 SNI 改写。
//
// 设计约束（见 docs/GHydra-PRD.md F3）：
//   - 只读不解密：仅解析 ClientHello 中的 server_name 扩展
//     （RFC 6066 §3 / RFC 8446 §4.1.2），不引入 utls 等第三方库
//   - 解析单次耗时 < 50µs（基准见 clienthello_test.go）
//   - 对畸形输入绝不 panic，只返回错误
package sni

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
)

// 解析与改写过程中的错误。调用方应将非 EOF 错误视为连接不可识别，
// 原样直连或拒绝，绝不重试解析。
var (
	ErrNotHandshake    = errors.New("sni: first TLS record is not a handshake record")
	ErrNotClientHello  = errors.New("sni: first handshake message is not a ClientHello")
	ErrMalformed       = errors.New("sni: malformed ClientHello")
	ErrNoSNI           = errors.New("sni: ClientHello contains no server_name extension")
	ErrRecordTooLarge  = errors.New("sni: TLS record length exceeds limit")
	ErrInvalidSNIValue = errors.New("sni: invalid replacement SNI value")
)

const (
	// TLS record content type: 0x16 = handshake (RFC 8446)
	contentTypeHandshake = 0x16
	// handshake message type: 0x01 = ClientHello
	handshakeTypeClientHi = 0x01
	// extension type: 0x0000 = server_name (RFC 6066 §3)
	extTypeServerName = 0x0000
	// 单条 TLS record 上限 16384 (RFC 8446)
	maxRecordLen = 1 << 14
	// 防御恶意长度字段的硬上限
	maxHandshakeMsgLen = 1 << 18
	// server_name 最长 255 字节 (RFC 6066)
	dnsNameMaxASCII = 255
)

// ClientHello 是从一条 TCP 流的首个 TLS record 中解析出的信息。
// Record 字段保留原始（或重组后的完整）record 字节，供转发器原样转发。
type ClientHello struct {
	Record        []byte
	LegacyVersion uint16
	SessionIDLen  int
	ServerName    string
	HasSNI        bool

	// 以下为改写 SNI 所需的长度字段在 Record 内的绝对偏移（不导出）。
	offRecordLen int
	offHSLen     int
	offExtsLen   int
	offExtLen    int
	offListLen   int
	offNameLen   int
	nameStart    int
	nameEnd      int
}

// ReadClientHello 从 r 中读取并解析首个 ClientHello。
//
// 支持跨多条 TLS record 分片的 ClientHello（读取时会精确消费到
// handshake message 结束为止，record 中可能粘连的后续字节留在 r 中，
// 交由调用方继续转发）。返回的 Record 恒为重组后的单条 record。
func ReadClientHello(r *bufio.Reader) (*ClientHello, error) {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	if hdr[0] != contentTypeHandshake {
		return nil, ErrNotHandshake
	}
	recLen := int(binary.BigEndian.Uint16(hdr[3:5]))
	if recLen == 0 || recLen > maxRecordLen {
		return nil, ErrRecordTooLarge
	}
	// 必须拷贝：hdr 会在后续 record 读取中被复用覆盖
	firstVer := []byte{hdr[1], hdr[2]}

	hs := make([]byte, 4)
	if _, err := io.ReadFull(r, hs); err != nil {
		return nil, err
	}
	if hs[0] != handshakeTypeClientHi {
		return nil, ErrNotClientHello
	}
	hsLen := int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3])
	if hsLen < 38 || hsLen > maxHandshakeMsgLen {
		return nil, ErrMalformed
	}

	payload := make([]byte, 0, 4+hsLen)
	payload = append(payload, hs...)
	remaining := hsLen
	for remaining > 0 {
		if len(payload) > 4 { // 后续 record 头
			if _, err := io.ReadFull(r, hdr); err != nil {
				return nil, err
			}
			if hdr[0] != contentTypeHandshake {
				return nil, ErrNotHandshake
			}
			recLen = int(binary.BigEndian.Uint16(hdr[3:5]))
			if recLen == 0 || recLen > maxRecordLen {
				return nil, ErrRecordTooLarge
			}
		}
		n := recLen
		if n > remaining {
			n = remaining
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(r, chunk); err != nil {
			return nil, err
		}
		payload = append(payload, chunk...)
		remaining -= n
		// recLen > n 时：record 尾部粘连的后续消息字节留在 r 中，不消费。
	}

	record := make([]byte, 0, 5+len(payload))
	record = append(record, contentTypeHandshake, firstVer[0], firstVer[1])
	record = binary.BigEndian.AppendUint16(record, uint16(len(payload)))
	record = append(record, payload...)
	return parse(record)
}

// parse 在一条完整 record 上做结构化解析，提取 SNI 与改写偏移。
func parse(record []byte) (*ClientHello, error) {
	if len(record) < 5 {
		return nil, ErrMalformed
	}
	recLen := int(binary.BigEndian.Uint16(record[3:5]))
	if 5+recLen != len(record) {
		return nil, ErrMalformed
	}
	payload := record[5:]
	if len(payload) < 4 || payload[0] != handshakeTypeClientHi {
		return nil, ErrNotClientHello
	}
	hsLen := int(payload[1])<<16 | int(payload[2])<<8 | int(payload[3])
	if 4+hsLen != len(payload) {
		return nil, ErrMalformed
	}

	ch := &ClientHello{
		Record:      record,
		offRecordLen: 3,
		offHSLen:    6, // record 头 5 + handshake type 1
	}
	body := payload[4:]
	if len(body) < 2+32+1 {
		return nil, ErrMalformed
	}
	ch.LegacyVersion = binary.BigEndian.Uint16(body[0:2])
	sidLen := int(body[34])
	ch.SessionIDLen = sidLen
	off := 35 + sidLen
	if off+2 > len(body) {
		return nil, ErrMalformed
	}
	csLen := int(binary.BigEndian.Uint16(body[off : off+2]))
	off += 2 + csLen
	if off+1 > len(body) {
		return nil, ErrMalformed
	}
	compLen := int(body[off])
	off += 1 + compLen
	if off+2 > len(body) {
		return nil, ErrMalformed
	}
	extsLen := int(binary.BigEndian.Uint16(body[off : off+2]))
	ch.offExtsLen = 9 + off // record 头 5 + handshake 头 4 + body 内偏移
	off += 2
	if off+extsLen > len(body) {
		return nil, ErrMalformed
	}
	exts := body[off : off+extsLen]

	eoff := 0
	for eoff+4 <= len(exts) {
		etype := binary.BigEndian.Uint16(exts[eoff : eoff+2])
		elen := int(binary.BigEndian.Uint16(exts[eoff+2 : eoff+4]))
		if eoff+4+elen > len(exts) {
			return nil, ErrMalformed
		}
		if etype == extTypeServerName && !ch.HasSNI {
			if err := ch.parseSNIExt(record, 9+off, eoff, exts[eoff+4:eoff+4+elen]); err != nil {
				return nil, err
			}
		}
		eoff += 4 + elen
	}
	return ch, nil
}

// parseSNIExt 解析 server_name 扩展体，记录改写所需的全部偏移。
// base 是 extensions 区在 record 内的绝对起点，eoff 是本扩展在其中的偏移。
func (ch *ClientHello) parseSNIExt(record []byte, base, eoff int, edata []byte) error {
	if len(edata) < 2 {
		return ErrMalformed
	}
	listLen := int(binary.BigEndian.Uint16(edata[0:2]))
	if 2+listLen != len(edata) {
		return ErrMalformed
	}
	extStart := base + eoff
	ch.offExtLen = extStart + 2
	ch.offListLen = extStart + 4
	p := 2
	for p+3 <= len(edata) {
		ntype := edata[p]
		nlen := int(binary.BigEndian.Uint16(edata[p+1 : p+3]))
		if p+3+nlen > len(edata) {
			return ErrMalformed
		}
		if ntype == 0 { // host_name（RFC 6066 只定义了这一种）
			ch.HasSNI = true
			ch.ServerName = string(edata[p+3 : p+3+nlen])
			ch.offNameLen = extStart + 4 + p + 1
			ch.nameStart = extStart + 4 + p + 3
			ch.nameEnd = ch.nameStart + nlen
			return nil
		}
		p += 3 + nlen
	}
	// 列表存在但无 host_name 条目：按无 SNI 处理
	return nil
}

// RewriteSNI 返回一条把 server_name 替换为 newName 的新 record 字节。
// 长度字段（record / handshake / extensions / extension / list / name）
// 全部按新名字长度重算。原 ClientHello 不被修改。
func (ch *ClientHello) RewriteSNI(newName string) ([]byte, error) {
	if !ch.HasSNI {
		return nil, ErrNoSNI
	}
	if len(newName) == 0 || len(newName) > dnsNameMaxASCII {
		return nil, ErrInvalidSNIValue
	}
	for i := 0; i < len(newName); i++ {
		if newName[i] == 0 {
			return nil, ErrInvalidSNIValue
		}
	}
	oldLen := ch.nameEnd - ch.nameStart
	delta := len(newName) - oldLen

	out := make([]byte, 0, len(ch.Record)+delta)
	out = append(out, ch.Record[:ch.nameStart]...)
	out = append(out, newName...)
	out = append(out, ch.Record[ch.nameEnd:]...)

	add16 := func(at int, d int) {
		v := int(binary.BigEndian.Uint16(out[at:at+2])) + d
		binary.BigEndian.PutUint16(out[at:at+2], uint16(v))
	}
	add24 := func(at int, d int) {
		v := int(out[at])<<16 | int(out[at+1])<<8 | int(out[at+2])
		v += d
		out[at] = byte(v >> 16)
		out[at+1] = byte(v >> 8)
		out[at+2] = byte(v)
	}
	add16(ch.offRecordLen, delta)
	add24(ch.offHSLen, delta)
	add16(ch.offExtsLen, delta)
	add16(ch.offExtLen, delta)
	add16(ch.offListLen, delta)
	binary.BigEndian.PutUint16(out[ch.offNameLen:ch.offNameLen+2], uint16(len(newName)))
	return out, nil
}
