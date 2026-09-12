package sni

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadFixture(t testing.TB, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	b, err := hex.DecodeString(string(raw))
	if err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	return b
}

// TestParseFixtures 验证各形态 ClientHello 的 SNI 提取。
func TestParseFixtures(t *testing.T) {
	cases := []struct {
		file    string
		wantSNI string
		wantHas bool
		wantVer uint16
	}{
		{"ch_normal_tls13.hex", "github.com", true, 0x0303},
		{"ch_tls12_sni.hex", "api.github.com", true, 0x0303},
		{"ch_no_sni.hex", "", false, 0x0303},
		{"ch_session_id.hex", "avatars.githubusercontent.com", true, 0x0303},
		{"ch_multi_ext_sni_middle.hex", "objects.githubusercontent.com", true, 0x0303},
		{"ch_long_sni.hex", strings.Repeat("a", 200) + ".githubusercontent.com", true, 0x0303},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			ch, err := ReadClientHello(bufio.NewReader(bytes.NewReader(loadFixture(t, c.file))))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if ch.HasSNI != c.wantHas || ch.ServerName != c.wantSNI {
				t.Errorf("SNI = %q (has=%v), want %q (has=%v)", ch.ServerName, ch.HasSNI, c.wantSNI, c.wantHas)
			}
			if ch.LegacyVersion != c.wantVer {
				t.Errorf("LegacyVersion = %#x, want %#x", ch.LegacyVersion, c.wantVer)
			}
		})
	}
}

// TestRecordIntact 解析出的 Record 必须与输入逐字节一致（转发器依赖此性质）。
func TestRecordIntact(t *testing.T) {
	for _, f := range []string{"ch_normal_tls13.hex", "ch_session_id.hex", "ch_multi_ext_sni_middle.hex"} {
		raw := loadFixture(t, f)
		ch, err := ReadClientHello(bufio.NewReader(bytes.NewReader(raw)))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if !bytes.Equal(ch.Record, raw) {
			t.Errorf("%s: Record 被修改", f)
		}
	}
}

// TestRewriteAndReparse 改写后重解析必须自洽（所有长度字段正确）。
func TestRewriteAndReparse(t *testing.T) {
	replacements := []string{"www.example.com", "a", "raw.githubusercontent.com"}
	for _, f := range []string{"ch_normal_tls13.hex", "ch_tls12_sni.hex", "ch_multi_ext_sni_middle.hex", "ch_long_sni.hex"} {
		raw := loadFixture(t, f)
		for _, repl := range replacements {
			ch, err := ReadClientHello(bufio.NewReader(bytes.NewReader(raw)))
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			out, err := ch.RewriteSNI(repl)
			if err != nil {
				t.Fatalf("%s rewrite %q: %v", f, repl, err)
			}
			ch2, err := ReadClientHello(bufio.NewReader(bytes.NewReader(out)))
			if err != nil {
				t.Fatalf("%s reparse %q: %v", f, repl, err)
			}
			if ch2.ServerName != repl {
				t.Errorf("%s: reparse SNI = %q, want %q", f, ch2.ServerName, repl)
			}
			// 除 SNI 外的其它字段必须保持不变
			if ch2.LegacyVersion != ch.LegacyVersion || ch2.SessionIDLen != ch.SessionIDLen {
				t.Errorf("%s: 字段被意外修改", f)
			}
			// 原对象不可变
			if !bytes.Equal(ch.Record, raw) {
				t.Errorf("%s: 原 Record 被改写污染", f)
			}
		}
	}
}

// TestRewriteNoSNI 无 SNI 的 ClientHello 改写必须返回 ErrNoSNI。
func TestRewriteNoSNI(t *testing.T) {
	ch, err := ReadClientHello(bufio.NewReader(bytes.NewReader(loadFixture(t, "ch_no_sni.hex"))))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ch.RewriteSNI("x.com"); err != ErrNoSNI {
		t.Errorf("err = %v, want ErrNoSNI", err)
	}
}

// TestRewriteInvalidValue 非法替换值必须被拒绝。
func TestRewriteInvalidValue(t *testing.T) {
	ch, err := ReadClientHello(bufio.NewReader(bytes.NewReader(loadFixture(t, "ch_normal_tls13.hex"))))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", string(make([]byte, 256)), "bad\x00name"} {
		if _, err := ch.RewriteSNI(bad); err != ErrInvalidSNIValue {
			t.Errorf("RewriteSNI(%q) err = %v, want ErrInvalidSNIValue", trunc(bad), err)
		}
	}
}

// TestTruncatedInputs 任何前缀截断必须报错而非 panic/成功。
func TestTruncatedInputs(t *testing.T) {
	for _, f := range []string{"ch_normal_tls13.hex", "ch_no_sni.hex", "ch_multi_ext_sni_middle.hex", "ch_long_sni.hex"} {
		raw := loadFixture(t, f)
		for i := 1; i < len(raw); i++ {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("%s[:%d] panic: %v", f, i, r)
					}
				}()
				if _, err := ReadClientHello(bufio.NewReader(bytes.NewReader(raw[:i]))); err == nil {
					t.Errorf("%s[:%d] 解析意外成功", f, i)
				}
			}()
		}
	}
}

// TestGarbageInputs 随机与全零输入绝不 panic。
func TestGarbageInputs(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	inputs := [][]byte{
		make([]byte, 512),
		make([]byte, 4096),
	}
	for i := range inputs[1] {
		inputs[1][i] = byte(rng.Intn(256))
	}
	buf := make([]byte, 512)
	rng.Read(buf)
	inputs = append(inputs, buf)
	for i, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("input[%d] panic: %v", i, r)
				}
			}()
			ch, err := ReadClientHello(bufio.NewReader(bytes.NewReader(in)))
			if err == nil && ch == nil {
				t.Errorf("input[%d]: nil result without error", i)
			}
		}()
	}
}

// TestStreamHasFollowingBytes TCP 粘包：ClientHello 后紧跟的额外字节必须留在 reader 中。
func TestStreamHasFollowingBytes(t *testing.T) {
	raw := loadFixture(t, "ch_normal_tls13.hex")
	stream := append(append([]byte{}, raw...), []byte("NEXT-RECORD-BYTES")...)
	r := bufio.NewReader(bytes.NewReader(stream))
	if _, err := ReadClientHello(r); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != "NEXT-RECORD-BYTES" {
		t.Errorf("残留字节 = %q", rest)
	}
}

// TestMultiRecordCH 分片到多条 record 的 ClientHello 必须能重组解析。
func TestMultiRecordCH(t *testing.T) {
	raw := loadFixture(t, "ch_normal_tls13.hex")
	bodyLen := int(raw[3])<<8 | int(raw[4])
	body := raw[5 : 5+bodyLen]
	// 拆成 3 条 record，最后一条多粘 8 字节后续消息
	split := [][]byte{body[:10], body[10:40], body[40:]}
	var stream []byte
	wrap := func(b []byte) []byte {
		out := []byte{0x16, 0x03, 0x01, byte(len(b) >> 8), byte(len(b))}
		return append(out, b...)
	}
	stream = append(stream, wrap(split[0])...)
	stream = append(stream, wrap(split[1])...)
	tail := append(append([]byte{}, split[2]...), []byte("TRAILING")...)
	stream = append(stream, wrap(tail)...)

	r := bufio.NewReader(bytes.NewReader(stream))
	ch, err := ReadClientHello(r)
	if err != nil {
		t.Fatal(err)
	}
	if ch.ServerName != "github.com" {
		t.Errorf("SNI = %q", ch.ServerName)
	}
	// 重组后的单条 record 重解析必须等价
	ch2, err := ReadClientHello(bufio.NewReader(bytes.NewReader(ch.Record)))
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if ch2.ServerName != ch.ServerName {
		t.Error("重组 record 重解析不等价")
	}
	// 后 8 字节留在流中
	rest, _ := io.ReadAll(r)
	if string(rest) != "TRAILING" {
		t.Errorf("残留 = %q, want TRAILING", rest)
	}
}

// TestRewriteMultiRecord 对分片输入改写，重解析仍自洽。
func TestRewriteMultiRecord(t *testing.T) {
	raw := loadFixture(t, "ch_normal_tls13.hex")
	bodyLen := int(raw[3])<<8 | int(raw[4])
	body := raw[5 : 5+bodyLen]
	var stream []byte
	for _, part := range [][]byte{body[:5], body[5:]} {
		rec := []byte{0x16, 0x03, 0x01, byte(len(part) >> 8), byte(len(part))}
		stream = append(append(stream, rec...), part...)
	}
	ch, err := ReadClientHello(bufio.NewReader(bytes.NewReader(stream)))
	if err != nil {
		t.Fatal(err)
	}
	out, err := ch.RewriteSNI("codeload.github.com")
	if err != nil {
		t.Fatal(err)
	}
	ch2, err := ReadClientHello(bufio.NewReader(bytes.NewReader(out)))
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if ch2.ServerName != "codeload.github.com" {
		t.Errorf("SNI = %q", ch2.ServerName)
	}
}

func trunc(s string) string {
	if len(s) > 16 {
		return fmt.Sprintf("%s...(len=%d)", s[:16], len(s))
	}
	return s
}

// BenchmarkParse 解析性能基线（PRD 验收: 单次 < 50µs，目标 < 5µs）。
func BenchmarkParse(b *testing.B) {
	raw := loadFixture(b, "ch_normal_tls13.hex")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ReadClientHello(bufio.NewReader(bytes.NewReader(raw))); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRewrite(b *testing.B) {
	raw := loadFixture(b, "ch_normal_tls13.hex")
	parsed, err := ReadClientHello(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parsed.RewriteSNI("www.example.com"); err != nil {
			b.Fatal(err)
		}
	}
}
