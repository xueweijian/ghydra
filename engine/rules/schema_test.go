package rules

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// 合法基线（快照式，子测试在此基础上做变异毒化）。
func validJSON(t *testing.T) string {
	t.Helper()
	gen := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	b, err := json.Marshal(&RulesFile{
		SchemaVersion: 1,
		Version:       42,
		GeneratedAt:   gen,
		ExpiresAt:     gen.Add(30 * 24 * time.Hour),
		Domains:       []string{"github.com", "*.github.com"},
		CDNEndpoints:  []string{"https://gh.1ciyuan.cn"},
		SeedIPs:       map[string][]string{"github.com": {"140.82.112.3", "20.205.243.166"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustReject(t *testing.T, data string, why string) {
	t.Helper()
	if _, err := ParseRules([]byte(data)); err == nil {
		t.Fatalf("accepted: %s", why)
	}
}

func TestParseRulesHappy(t *testing.T) {
	rf, err := ParseRules([]byte(validJSON(t)))
	if err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	if rf.Version != 42 || len(rf.Domains) != 2 || rf.SeedIPs["github.com"][0] != "140.82.112.3" {
		t.Fatalf("field mismatch: %+v", rf)
	}
}

func TestRejectUnknownFields(t *testing.T) {
	s := validJSON(t)
	s = strings.Replace(s, `{"`, `{"evil_extra":1,"`, 1)
	mustReject(t, s, "unknown field")
}

func TestRejectDuplicateKeys(t *testing.T) {
	base := validJSON(t)
	// 顶层重复 version：DisallowUnknownFields 抓不到，dupKeys 必须抓。
	s := strings.Replace(base, `"version":42`, `"version":42,"version":43`, 1)
	mustReject(t, s, "duplicate top-level key")
	// 嵌套对象重复键（seed_ips 内域名重复）。
	s2 := strings.Replace(base, `"github.com": [`, `"github.com": [`+"", 1)
	dup := `{"schema_version":1,"version":1,"generated_at":"2026-09-13T00:00:00Z","expires_at":"2026-10-13T00:00:00Z",` +
		`"domains":["github.com"],"cdn_endpoints":[],"seed_ips":{"a.com":["1.2.3.4"],"a.com":["5.6.7.8"]}}`
	_ = s2
	mustReject(t, dup, "duplicate nested key")
}

func TestRejectTrailingData(t *testing.T) {
	mustReject(t, validJSON(t)+`{"schema_version":1}`, "trailing object")
}

func TestRejectSchemaVersionDrift(t *testing.T) {
	s := strings.Replace(validJSON(t), `"schema_version":1`, `"schema_version":2`, 1)
	mustReject(t, s, "schema v2 to v1 client (A9)")
}

func TestRejectVersionBounds(t *testing.T) {
	mustReject(t, strings.Replace(validJSON(t), `"version":42`, `"version":0`, 1), "version 0")
	mustReject(t, strings.Replace(validJSON(t), `"version":42`, `"version":1000001`, 1), "version over max (A4)")
}

func TestRejectTimestamps(t *testing.T) {
	base := validJSON(t)
	// 过长有效期。
	s := strings.Replace(base,
		`"expires_at":"2026-10-13T00:00:00Z"`, `"expires_at":"2027-10-13T00:00:00Z"`, 1)
	mustReject(t, s, "validity > 45d")
	s = strings.Replace(base, `"expires_at":"2026-10-13T00:00:00Z"`, `"expires_at":"2026-09-12T00:00:00Z"`, 1)
	mustReject(t, s, "expires before generated")
	// 缺时间戳。
	mustReject(t, `{"schema_version":1,"version":1,"domains":["github.com"]}`, "missing timestamps")
}

func TestRejectDomainPoison(t *testing.T) {
	base := validJSON(t)
	cases := map[string]string{
		"loopback":       "localhost",
		"reserved local": "foo.local",
		"internal":       "svc.internal",
		"home.arpa":      "x.home.arpa",
		"invalid tld":    "not-a-domain",
		"underline":      "under_score.com",
		"space":          "github .com",
		"empty":          "",
	}
	for why, dom := range cases {
		poison := `"github.com","` + dom + `"`
		s := strings.Replace(base, `"github.com","*.github.com"`, poison, 1)
		mustReject(t, s, "domain poison: "+why)
	}
	// 超量（65 条）。
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 65; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"d`)
		b.WriteString(strings.Repeat("x", 1))
		b.WriteString(`.example.com"`)
	}
	s := strings.Replace(base, `"github.com","*.github.com"`, strings.TrimSuffix(b.String(), ",")[0:0]+b.String(), 1)
	mustReject(t, s, "65 domains")
}

func TestRejectCDNPoison(t *testing.T) {
	base := validJSON(t)
	cases := map[string]string{
		"plain http": "http://gh-proxy.com",
		"space":      "https://gh-proxy.com /x",
		"bad host":   "https://not_a_domain",
		"loopback":   "https://localhost",
	}
	for why, cdn := range cases {
		s := strings.Replace(base, `"https://gh.1ciyuan.cn"`, `"`+cdn+`"`, 1)
		mustReject(t, s, "cdn poison: "+why)
	}
	// 超量（5 条）。
	s := strings.Replace(base, `"https://gh.1ciyuan.cn"`,
		`"https://a.com","https://b.com","https://c.com","https://d.com","https://e.com"`, 1)
	mustReject(t, s, "5 cdn endpoints")
}

func TestRejectSeedIPPoison(t *testing.T) {
	base := validJSON(t)
	cases := map[string]string{
		"private":     "192.168.1.1",
		"loopback":    "127.0.0.1",
		"link local":  "169.254.1.1",
		"multicast":   "224.0.0.1",
		"unspecified": "0.0.0.0",
		"not ip":      "github.com",
	}
	for why, ip := range cases {
		s := strings.Replace(base, `"140.82.112.3"`, `"`+ip+`"`, 1)
		mustReject(t, s, "seed ip poison: "+why)
	}
	// 单域 33 个 IP。
	ips := make([]string, 33)
	for i := range ips {
		ips[i] = "140.82.112.3"
	}
	poison, _ := json.Marshal(ips)
	s := strings.Replace(base, `["140.82.112.3","20.205.243.166"]`, string(poison), 1)
	mustReject(t, s, "33 seed ips")
}

func TestTooLarge(t *testing.T) {
	big := make([]byte, MaxFileBytes+1)
	if _, err := ParseRules(big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}
