// ghydra CLI — M0 PoC：SNI 转发器 + 自举链基准。
//
// 用法:
//
//	ghydra bench [--json]          四级自举链探测，输出轨迹报告
//	ghydra poc [--listen L] [--rewrite-sni NAME]   本地 SNI 转发器
//
// M0 验证目标见 docs/GHydra-PRD.md §8 M0。M1 起将替换为 cobra 子命令结构。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/xueweijian/ghydra/engine/bootstrap"
	"github.com/xueweijian/ghydra/engine/sni"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "bench":
		benchCmd(os.Args[2:])
	case "poc":
		pocCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ghydra (M0 PoC)

用法:
  ghydra bench [--json]                     四级自举链探测报告
  ghydra poc [--listen ADDR] [--rewrite-sni NAME]  本地 SNI 转发器
`)
}

func benchCmd(args []string) {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "输出 JSON（真机验收报告格式）")
	timeout := fs.Duration("timeout", 30*time.Second, "总超时")
	_ = fs.Parse(args)

	r := bootstrap.New()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res := r.Resolve(ctx)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			log.Fatal(err)
		}
		return
	}
	fmt.Printf("四级自举链报告  总耗时 %.1fms\n", res.ElapsedMS)
	fmt.Printf("来源: %s  候选 IP: %d 个\n", res.Source, len(res.IPs))
	for _, s := range res.Steps {
		status := "FAIL"
		if s.OK {
			status = "OK"
		}
		fmt.Printf("  L%d %-45s %-4s %6.1fms  err=%s\n", s.Level, s.Name, status, s.ElapsedMS, s.Err)
	}
	for _, ip := range res.IPs {
		fmt.Printf("    %s\n", ip)
	}
}

func pocCmd(args []string) {
	fs := flag.NewFlagSet("poc", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8443", "本地监听地址")
	rewrite := fs.String("rewrite-sni", "", "可选：把 ClientHello 的 SNI 改写为该值（domain fronting 实验）")
	dialTimeout := fs.Duration("dial-timeout", 5*time.Second, "上游连接超时")
	_ = fs.Parse(args)

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("监听失败: %v", err)
	}
	var conns int64
	log.Printf("GHydra PoC 转发器已启动: %s (rewrite-sni=%q)", *listen, *rewrite)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		id := atomic.AddInt64(&conns, 1)
		go func(c net.Conn) {
			defer c.Close()
			relay(c, *rewrite, *dialTimeout, id)
		}(conn)
	}
}

func relay(conn net.Conn, rewrite string, dialTimeout time.Duration, id int64) {
	start := time.Now()
	br := bufio.NewReader(conn)
	ch, err := sni.ReadClientHello(br)
	if err != nil {
		log.Printf("#%d ClientHello 解析失败: %v", id, err)
		return
	}
	host := ch.ServerName
	out := ch.Record
	if rewrite != "" {
		out, err = ch.RewriteSNI(rewrite)
		if err != nil {
			log.Printf("#%d SNI 改写失败: %v", id, err)
			return
		}
		host = rewrite
	}
	if host == "" {
		log.Printf("#%d 无 SNI，拒绝转发", id)
		return
	}
	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, "443"), dialTimeout)
	if err != nil {
		log.Printf("#%d %s 上游连接失败: %v", id, host, err)
		return
	}
	defer upstream.Close()
	if _, err := upstream.Write(out); err != nil {
		log.Printf("#%d %s 写入上游失败: %v", id, host, err)
		return
	}
	log.Printf("#%d SNI=%s -> %s (改写=%t)", id, ch.ServerName, upstream.RemoteAddr(), rewrite != "")
	go func() {
		io.Copy(upstream, br)
		if tc, ok := upstream.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			upstream.Close()
		}
	}()
	n, _ := io.Copy(conn, upstream)
	log.Printf("#%d 完成 %s 下行 %dB 耗时 %s", id, ch.ServerName, n, time.Since(start).Round(time.Millisecond))
}
