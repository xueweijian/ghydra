// fakesite 是故障演练的黑盒源站工具（M2-W4 drill / M3-W2 毒化矩阵）。
//
//	-mode tlsdead  : accept 后立即关闭（TLS 握手死特征）
//	-mode rst      : accept 后以 RST 关闭（SO_LINGER 0）
//	-mode blackhole: accept 后挂起不响应
//	-mode cdn      : 明文 HTTP，任意路径返回 200 + 固定内容（模拟 B 通道静态资产）
//	-mode 403      : 自签 TLS + 源头 403（源级故障；需配 -cert 信任才可用）
//	-mode rules    : 规则向量伺服（M3-W2 phase 3）：-rules-dir 下按 URL
//	                 相对路径伺服 current.json(.minisig)；-poison 注入毒化：
//	                 a1 内容改一字节（签名脱钩）/ a5 chunked 无限流（无尽数据）。
//
// 仅为演练/测试造数，不进 ghydra 主程序。
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	mode := flag.String("mode", "tlsdead", "tlsdead|rst|blackhole|cdn|rules")
	port := flag.Int("port", 443, "监听端口")
	rulesDir := flag.String("rules-dir", "", "rules 模式：向量根目录（URL 路径映射其下相对路径）")
	poison := flag.String("poison", "", "rules 模式毒化开关：a1|a5")
	flag.Parse()
	addr := fmt.Sprintf("127.0.0.1:%d", *port)

	switch *mode {
	case "cdn":
		h := http.NewServeMux()
		h.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte("GHYDRA-DRILL-ASSET-0123456789ABCDEF"))
		})
		log.Printf("fakesite cdn on %s", addr)
		log.Fatal(http.ListenAndServe(addr, h))
	case "rules":
		if *rulesDir == "" {
			log.Fatal("rules 模式需要 -rules-dir")
		}
		if *poison != "" && *poison != "a1" && *poison != "a5" {
			log.Fatalf("未知毒化变体 %q（a1|a5）", *poison)
		}
		h := http.NewServeMux()
		h.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			// 路径净化：只允许 rules-dir 内相对路径
			rel := filepath.Clean("/" + r.URL.Path)
			full := filepath.Join(*rulesDir, rel)
			if !strings.HasPrefix(full, filepath.Clean(*rulesDir)) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			// A5 无尽数据：大块无 sleep 填充（读端限长 1MiB 处断开，
			// 写端 broken pipe 自然退出）——同时检验慢速流/快速洪泛
			// 两种形态的读侧防线。
			if *poison == "a5" && strings.HasSuffix(r.URL.Path, "current.json") {
				w.Header().Set("Content-Type", "application/json")
				flusher := w.(http.Flusher)
				flusher.Flush()
				chunk := make([]byte, 1<<20) // 1 MiB
				for i := 0; i < 64; i++ {    // 上限 64 MiB
					if _, err := w.Write(chunk); err != nil {
						return
					}
					flusher.Flush()
				}
				return
			}
			body, err := os.ReadFile(full)
			if err != nil {
				log.Printf("rules 404: path=%q rel=%q full=%q", r.URL.Path, rel, full)
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			// A1 内容投毒：json 首个 '{' 后一字节翻转（签名脱钩，验签必拒）
			if *poison == "a1" && strings.HasSuffix(r.URL.Path, "current.json") && len(body) > 2 {
				body = append([]byte{}, body...)
				body[2] ^= 0x01
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(body)
		})
		log.Printf("fakesite rules on %s dir=%s poison=%q", addr, *rulesDir, *poison)
		log.Fatal(http.ListenAndServe(addr, h))
	case "403":
		log.Fatal("403 模式需要 TLS 证书装配（drill v1 未用，预留）")
	default:
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatalf("listen %s: %v", addr, err)
		}
		log.Printf("fakesite %s on %s", *mode, addr)
		for {
			c, err := ln.Accept()
			if err != nil {
				log.Fatal(err)
			}
			switch *mode {
			case "tlsdead":
				c.Close()
			case "rst":
				raw, ok := c.(*net.TCPConn)
				if ok {
					_ = raw.SetLinger(0) // 关闭时发 RST
				}
				c.Close()
			case "blackhole":
				go func(c net.Conn) {
					time.Sleep(10 * time.Minute)
					c.Close()
				}(c)
			default:
				log.Fatalf("未知模式 %s", *mode)
			}
		}
	}
}
