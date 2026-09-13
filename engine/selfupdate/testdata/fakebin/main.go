package main

// fakebin：自更新自检的替身二进制（L2 集成测试）。行为由环境驱动：
//
//	FAKE_MODE=broken  → exit 1（坏新版：自检必败）
//	FAKE_MODE=hang    → 休眠 60s（超时路径）
//	其余              → `ghydra version` 语义：打印 "ghydra version $FAKE_VERSION"
//
// 由 update_test.go 的 TestMain `go build` 产出，进 staging archive 充当
// "新版 ghydra"——自检跑真实子进程，不做内存模拟。
import (
	"fmt"
	"os"
	"time"
)

func main() {
	switch os.Getenv("FAKE_MODE") {
	case "broken":
		os.Exit(1)
	case "hang":
		time.Sleep(60 * time.Second)
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("ghydra version %s\n", os.Getenv("FAKE_VERSION"))
		return
	}
	os.Exit(3)
}
