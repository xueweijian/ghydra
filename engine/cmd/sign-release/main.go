// sign-release —— Release checksums.txt 的 minisign 签名 + 上传（W4p2 D5）。
//
// 签名流程（拍板 §8-1：私钥永不出用户侧/沙箱，CI 零 secret）：
//
//	CI release.yml 出包到 **draft Release** → 本工具在本地/沙箱跑：
//	  1. 拉取 draft release 的 checksums.txt
//	  2. SignDetached 签名（与 selfupdate 验签链同算法，官方 minisign -V 可验）
//	  3. 上传 checksums.txt.minisig → （--publish 时）draft → published
//
// 用法：
//
//	GH_TOKEN=xxx go run ./scripts/sign-release -key <minisign.key> -tag v0.9.0-rc1 [-publish]
//
// 信任链对齐 engine/selfupdate（U1-U8）：SelectAsset 要求 checksums.txt.minisig
// 与 checksums.txt 同时在位，验签密钥 = engine/rules/keys.go 同源体系之外的
// release 专用钥匙（F0716070E4E793E7，shared/ghydra-keys/）。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/xueweijian/ghydra/engine/internal/minisign"
)

const apiBase = "https://api.github.com/repos/xueweijian/ghydra"

type release struct {
	ID      int     `json:"id"`
	TagName string  `json:"tag_name"`
	Draft   bool    `json:"draft"`
	Assets  []asset `json:"assets"`
	HtmlURL string  `json:"html_url"`
}

type asset struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func main() {
	keyPath := flag.String("key", "", "release minisign 私钥路径（必填）")
	tag := flag.String("tag", "", "release tag（必填）")
	publish := flag.Bool("publish", false, "签名上传后把 draft 转 published")
	apiOverride := flag.String("api", apiBase, "API base（演练可指 fake server）")
	flag.Parse()
	if *keyPath == "" || *tag == "" {
		fmt.Fprintln(os.Stderr, "usage: sign-release -key <key> -tag <tag> [-publish] [-api url]")
		os.Exit(2)
	}
	token := os.Getenv("GH_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "GH_TOKEN 未设置")
		os.Exit(2)
	}

	sk, err := minisign.ParseSecretKeyFile(*keyPath)
	if err != nil {
		fatal("解析私钥: %v", err)
	}

	rel, err := findRelease(token, *apiOverride, *tag)
	if err != nil {
		fatal("定位 release %s: %v", *tag, err)
	}
	if *publish && !rel.Draft {
		fmt.Println("release 已是 published，跳过 publish 步骤")
		*publish = false
	}

	sums := fetchAsset(token, *apiOverride, rel, "checksums.txt")
	sig, err := minisign.SignDetached(sk, sums,
		"GHydra release checksums "+*tag,
		"trusted: "+*tag)
	if err != nil {
		fatal("签名: %v", err)
	}

	if err := uploadAsset(token, *apiOverride, rel.ID, "checksums.txt.minisig", sig); err != nil {
		fatal("上传 minisig: %v", err)
	}
	fmt.Printf("OK checksums.txt.minisig 已上传 → %s\n", rel.HtmlURL)

	if *publish {
		if err := publishRelease(token, *apiOverride, rel.ID); err != nil {
			fatal("publish: %v", err)
		}
		fmt.Println("OK release 已发布")
	}
}

func findRelease(token, base, tag string) (*release, error) {
	// 先试 tags 端点（published）；**该端点不返回 draft**——draft 需遍历
	// /releases 按 tag+draft 匹配（rc1 演练实证：同 tag 双 release 时
	// tags 端点命中 published 旧格式，拿不到 checksums.txt）。
	req, _ := http.NewRequest("GET", base+"/releases/tags/"+tag, nil)
	auth(req, token)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			var rel release
			if err := json.NewDecoder(resp.Body).Decode(&rel); err == nil && hasAsset(&rel, "checksums.txt") {
				return &rel, nil
			}
		}
	}
	// draft 遍历
	req2, _ := http.NewRequest("GET", base+"/releases?per_page=30", nil)
	auth(req2, token)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		return nil, err
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		return nil, fmt.Errorf("列 releases: HTTP %d", resp2.StatusCode)
	}
	var rels []release
	if err := json.NewDecoder(resp2.Body).Decode(&rels); err != nil {
		return nil, err
	}
	for i := range rels {
		if rels[i].Draft && tagFor(&rels[i]) == tag && hasAsset(&rels[i], "checksums.txt") {
			return &rels[i], nil
		}
	}
	return nil, fmt.Errorf("未找到含 checksums.txt 的 release（tag=%s，含 draft）", tag)
}

func hasAsset(rel *release, name string) bool {
	for _, a := range rel.Assets {
		if a.Name == name {
			return true
		}
	}
	return false
}

// tagFor release 对象没有 tag_name 字段注入（列表接口有）——release 结构
// 已含 TagName（json tag_name），列表解码会填充。
func tagFor(rel *release) string { return rel.TagName }

func fetchAsset(token, base string, rel *release, name string) []byte {
	for _, a := range rel.Assets {
		if a.Name != name {
			continue
		}
		req, _ := http.NewRequest("GET", base+"/releases/assets/"+fmt.Sprint(a.ID), nil)
		auth(req, token)
		req.Header.Set("Accept", "application/octet-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fatal("下载 %s: %v", name, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			fatal("下载 %s: HTTP %d", name, resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
		if err != nil {
			fatal("读 %s: %v", name, err)
		}
		return b
	}
	fatal("release 缺资产 %s", name)
	return nil
}

func uploadAsset(token, base string, relID int, name string, data []byte) error {
	req, _ := http.NewRequest("POST",
		strings.Replace(base, "https://api.github.com", "https://uploads.github.com", 1)+
			fmt.Sprintf("/releases/%d/assets?name=%s", relID, name),
		bytes.NewReader(data))
	auth(req, token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	return nil
}

func publishRelease(token, base string, relID int) error {
	body := strings.NewReader(`{"draft": false}`)
	req, _ := http.NewRequest("PATCH", base+"/releases/"+fmt.Sprint(relID), body)
	auth(req, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	return nil
}

func auth(r *http.Request, token string) {
	r.Header.Set("Authorization", "token "+token)
	r.Header.Set("Accept", "application/vnd.github+json")
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "sign-release: "+f+"\n", a...)
	os.Exit(1)
}
