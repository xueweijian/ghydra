package diag

import (
	"strings"
	"testing"
)

// L1 表驱动（设计 §4 冻结：token/家目录/用户名/代理凭据 各 ≥3 向量；
// 非敏感不误伤——IP 必须保留）。
func TestRedactTable(t *testing.T) {
	const home = "/home/zhang3"
	cases := []struct {
		name string
		in   string
		want []string // 必须出现在 Redact 结果中
		not  []string // 必须不出现
	}{
		// —— 家目录（≥3 向量）——
		{"home-路径", home + "/.ghydra/serve.log", []string{"~/.ghydra/serve.log"}, []string{home}},
		{"home-单独出现", home + " 是主目录", []string{"~ 是主目录"}, []string{home}},
		{"home-多行重复", "a\n" + home + "/x\nb\n" + home + "/y", []string{"~/x", "~/y"}, []string{home}},
		// —— token 键值对（≥3 向量）——
		{"token-冒号json", `{"api_token": "0123456789abcdef0123456789abcdef"}`, []string{"<redact>"}, []string{"0123456789abcdef0123456789abcdef"}},
		{"token-等号flag", "--api-token=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []string{"<redact>"}, []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		{"token-secret键", `password: "hunter2secret"`, []string{"<redact>"}, []string{"hunter2secret"}},
		// —— 裸 32 hex（api-token 文件形态）——
		{"bare-32hex", "认证失败 tok=0123456789abcdef0123456789abcdef 拒绝", []string{"<redact-32hex>"}, []string{"0123456789abcdef0123456789abcdef"}},
		{"bare-32hex-大写", "ABCDEF0123456789ABCDEF0123456789", []string{"<redact-32hex>"}, []string{"ABCDEF0123456789ABCDEF0123456789"}},
		// —— URL 凭据（≥3 向量）——
		{"url-凭据http", "via http://zhang3:s3cret@127.0.0.1:9801", []string{"http://<redact>@127.0.0.1:9801"}, []string{"s3cret"}},
		{"url-凭据socks5", "socks5://u:p@proxy:1080", []string{"socks5://<redact>@proxy:1080"}, []string{":p@"}},
		{"url-无凭据保留", "https://github.com/xueweijian/ghydra", []string{"https://github.com/xueweijian/ghydra"}, nil},
		// —— Bearer ——
		{"bearer", "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9", []string{"Bearer <redact>"}, []string{"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"}},
		// —— 非敏感不误伤（IP/域名/时长/短hex）——
		{"ip-保留", "dial 20.205.243.166:443 ok in 87ms", []string{"20.205.243.166:443", "87ms"}, nil},
		{"域名-保留", "GET https://api.github.com/repos 200", []string{"api.github.com"}, nil},
		{"短hex-保留", "commit abc1234 (7位) 不脱敏", []string{"abc1234"}, nil},
		{"40hex-sha不误伤", "sha 0123456789abcdef0123456789abcdef01234567 完整提交号", []string{"0123456789abcdef0123456789abcdef01234567"}, []string{"<redact-32hex>"}},
		{"用户名-普通词不误伤", "the user agent string here", []string{"the user agent string here"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(home, tc.in)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("缺 %q\ngot: %s", w, got)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("泄漏 %q\ngot: %s", n, got)
				}
			}
		})
	}
}

// 幂等：Redact 再跑一遍结果不变（二次防泄漏场景）。
func TestRedactIdempotent(t *testing.T) {
	in := "/home/zhang3 tok=0123456789abcdef0123456789abcdef http://u:p@x"
	once := Redact("/home/zhang3", in)
	twice := Redact("/home/zhang3", once)
	if once != twice {
		t.Errorf("不幂等:\nonce:  %s\ntwice: %s", once, twice)
	}
}
