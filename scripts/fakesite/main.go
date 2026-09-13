// fakesite 是故障演练的黑盒源站工具（M2-W4 drill）。
//
//   -mode tlsdead  : accept 后立即关闭（TLS 握手死特征）
//   -mode rst      : accept 后以 RST 关闭（SO_LINGER 0）
//   -mode blackhole: accept 后挂起不响应
//   -mode cdn      : 明文 HTTP，任意路径返回 200 + 固定内容（模拟 B 通道静态资产）
//   -mode 403      : 自签 TLS + 源头 403（源级故障；需配 -cert 信任才可用）
//
// 仅为演练/测试造数，不进 ghydra 主程序。
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

func main() {
	mode := flag.String("mode", "tlsdead", "tlsdead|rst|blackhole|cdn")
	port := flag.Int("port", 443, "监听端口")
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
