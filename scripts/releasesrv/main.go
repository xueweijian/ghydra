// releasesrv 冒烟/演练用 fake Release server（W4）。
//
// 从一个本地"新版二进制"动态构造 GitHub Release 形态：
//
//	GET <任意前缀>/releases/latest  → Release JSON（资产 URL 指回本机）
//	GET /files/<name>               → 资产字节
//
// 三件套 = 平台 archive（tar.gz/zip，内含 ghydra）+ checksums.txt +
// checksums.txt.minisig（用传入的 minisign -W 私钥签名——冒烟用 repo 内
// 测试钥匙，生产钥匙永不离线介质）。
//
// 攻击模式（--tamper-*）：资产/清单在**签名之后**被篡改——防线必须当场识破。
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/crypto/blake2b"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:19711", "监听地址")
	newbin := flag.String("newbin", "", "新版 ghydra 二进制（打进 archive）")
	version := flag.String("version", "v1.0.1", "Release tag")
	keyPath := flag.String("key", "", "minisign -W 私钥（签名 checksums）")
	tamperAsset := flag.Bool("tamper-asset", false, "资产在签名后篡改（U1 第一防线演练）")
	tamperChecksums := flag.Bool("tamper-checksums", false, "checksums 在签名后篡改（U1 第二防线演练）")
	wrongSize := flag.Bool("wrong-size", false, "API 元数据尺寸报错（U4 演练）")
	flag.Parse()
	if *newbin == "" || *keyPath == "" {
		log.Fatal("须提供 -newbin 与 -key")
	}

	sk, pub, keyID, err := loadKey(*keyPath)
	if err != nil {
		log.Fatal(err)
	}

	bin, err := os.ReadFile(*newbin)
	if err != nil {
		log.Fatal(err)
	}
	exe := "ghydra"
	if runtime.GOOS == "windows" {
		exe = "ghydra.exe"
	}
	archiveName := fmt.Sprintf("ghydra-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		archiveName += ".zip"
	} else {
		archiveName += ".tar.gz"
	}
	archive := buildArchive(exe, bin)

	// 签名基于原始内容；篡改只影响 server 侧字节。
	sum := sha256.Sum256(archive)
	csBody := fmt.Sprintf("%s  %s\n%s  checksums.txt\n",
		hex.EncodeToString(sum[:]), archiveName, strings.Repeat("b", 64))
	sig := signED(sk, keyID, []byte(csBody), "ghydra release "+*version)

	serveArc := archive
	if *tamperAsset {
		serveArc = flipByte(archive, 3)
	}
	serveCS := []byte(csBody)
	if *tamperChecksums {
		serveCS = bytes.Replace(serveCS, []byte("checksums.txt\n"), []byte("checksums.txX\n"), 1)
	}

	addr := *listen
	tag := *version
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/files/"):
			name := strings.TrimPrefix(r.URL.Path, "/files/")
			var data []byte
			switch name {
			case archiveName:
				data = serveArc
			case "checksums.txt":
				data = serveCS
			case "checksums.txt.minisig":
				data = sig
			default:
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			w.Write(data)
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			assets := []map[string]any{}
			for _, name := range []string{archiveName, "checksums.txt", "checksums.txt.minisig"} {
				var size int64
				switch name {
				case archiveName:
					size = int64(len(serveArc))
					if *wrongSize {
						size += 4096
					}
				case "checksums.txt":
					size = int64(len(serveCS))
				default:
					size = int64(len(sig))
				}
				assets = append(assets, map[string]any{
					"name": name, "size": size,
					"browser_download_url": fmt.Sprintf("http://%s/files/%s", addr, name),
				})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": tag, "draft": false, "prerelease": false,
				"body": "smoke release", "assets": assets,
			})
		default:
			http.NotFound(w, r)
		}
	})
	log.Printf("releasesrv %s on http://%s（archive=%s tamperAsset=%v tamperChecksums=%v wrongSize=%v）",
		*version, addr, archiveName, *tamperAsset, *tamperChecksums, *wrongSize)
	log.Printf("公钥 b64: %s", base64.StdEncoding.EncodeToString(pub))
	log.Fatal(http.ListenAndServe(addr, mux))
}

func flipByte(b []byte, i int) []byte {
	out := append([]byte{}, b...)
	out[i] ^= 0xff
	return out
}

func buildArchive(name string, bin []byte) []byte {
	var buf bytes.Buffer
	if strings.HasSuffix(filepath.Base(os.Args[0]), ".exe") || runtime.GOOS == "windows" {
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create(name)
		w.Write(bin)
		zw.Close()
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg})
	tw.Write(bin)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// loadKey 解析 minisign -W 私钥（158B 布局，同 engine/internal/minisign——
// 本工具 standalone 引用避免 main 包互相依赖）。
func loadKey(path string) (ed25519.PrivateKey, ed25519.PublicKey, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\r\n"), "\n")
	if len(lines) < 2 {
		return nil, nil, nil, fmt.Errorf("私钥文件格式非法")
	}
	blob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[1]))
	if err != nil || len(blob) != 158 || string(blob[0:2]) != "Ed" {
		return nil, nil, nil, fmt.Errorf("仅支持 -W 无密码 158B 私钥")
	}
	sk := ed25519.PrivateKey(append([]byte{}, blob[62:126]...))
	pubAny, _ := sk.Public().(ed25519.PublicKey)
	if !bytes.Equal(blob[94:126], pubAny) {
		return nil, nil, nil, fmt.Errorf("私钥 seed/pk 不自洽")
	}
	return sk, pubAny, blob[54:62], nil
}

// signED minisign 0.11 "ED"（blake2b-512 prehash）四行签名。
func signED(sk ed25519.PrivateKey, keyID []byte, data []byte, trusted string) []byte {
	h := blake2b.Sum512(data)
	sig := ed25519.Sign(sk, h[:])
	global := ed25519.Sign(sk, append(append([]byte{}, sig...), []byte(trusted)...))
	blob := append(append([]byte("ED"), keyID...), sig...)
	var b strings.Builder
	fmt.Fprintf(&b, "untrusted comment: ghydra release smoke\n")
	b.WriteString(base64.StdEncoding.EncodeToString(blob))
	b.WriteByte('\n')
	fmt.Fprintf(&b, "trusted comment: %s\n", trusted)
	b.WriteString(base64.StdEncoding.EncodeToString(global))
	b.WriteByte('\n')
	return []byte(b.String())
}
