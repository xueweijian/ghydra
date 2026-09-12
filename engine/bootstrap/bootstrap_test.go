package bootstrap

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// brokenHTTPClient 返回一个必然连接失败的客户端（指向拒绝连接的本地端口）。
func brokenHTTPClient() *http.Client {
	proxyURL, _ := url.Parse("http://127.0.0.1:1")
	return &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		Timeout:   2 * time.Second,
	}
}

func serveFixture(t *testing.T, path string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read fixture: %v", err)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDefaultSeeds(t *testing.T) {
	s, err := DefaultSeeds()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.SeedIPs) == 0 {
		t.Error("嵌入种子为空")
	}
	found := false
	for _, d := range s.Domains {
		if d == "*.github.com" {
			found = true
		}
	}
	if !found {
		t.Error("种子域名清单缺 *.github.com")
	}
}

func TestExtractCIDRIPs(t *testing.T) {
	got := extractCIDRIPs([]string{"192.30.252.0/22"}, 2)
	want := []string{"192.30.252.1", "192.30.252.2", "192.30.255.255"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// /32 不产出（无主机位）
	if got := extractCIDRIPs([]string{"20.201.28.151/32"}, 4); len(got) != 0 {
		t.Errorf("/32 got %v, want empty", got)
	}
	// IPv6 段被跳过
	if got := extractCIDRIPs([]string{"2606:50c0::/32"}, 4); len(got) != 0 {
		t.Errorf("v6 got %v, want empty", got)
	}
	// 非法输入被跳过
	if got := extractCIDRIPs([]string{"not-a-cidr"}, 4); len(got) != 0 {
		t.Errorf("garbage got %v, want empty", got)
	}
}

func TestMetaFetch(t *testing.T) {
	srv := serveFixture(t, "testdata/meta_sample.json")
	r := &Resolver{HTTP: srv.Client(), MetaURL: srv.URL, PerNet: 2}
	ips, doms, err := r.fetchMetaWith(context.Background(), r.HTTP)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"192.30.252.1", "192.30.252.2", "192.30.255.255",
		"185.199.108.1", "185.199.108.2", "185.199.111.255",
		"140.82.112.1", "140.82.112.2", "140.82.127.255",
	}
	if !reflect.DeepEqual(ips, want) {
		t.Errorf("ips = %v", ips)
	}
	if !reflect.DeepEqual(doms, []string{"*.github.com", "*.githubassets.com", "*.githubusercontent.com"}) {
		t.Errorf("domains = %v", doms)
	}
}

func TestResolveDoHAddr(t *testing.T) {
	cases := []struct{ in, want string }{
		{"dns.alidns.com:443", "223.5.5.5:443"},
		{"doh.pub:443", "119.29.29.29:443"},
		{"[2606:50c0::1]:443", "[2606:50c0::1]:443"},
		{"unknown.example.com:443", "unknown.example.com:443"},
		{"no-port.example.com", "no-port.example.com"},
	}
	for _, c := range cases {
		if got := resolveDoHAddr(c.in, dohDialMap); got != c.want {
			t.Errorf("resolveDoHAddr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDoHFetch(t *testing.T) {
	srv := serveFixture(t, "testdata/doh_sample.json")
	r := &Resolver{HTTP: srv.Client(), DoHName: "github.com"}
	ips, err := r.fetchDoH(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// 只取 A 记录，CNAME(type 5) 与 AAAA(type 28) 必须被过滤
	if !reflect.DeepEqual(ips, []string{"20.205.243.166", "20.201.28.151"}) {
		t.Errorf("ips = %v", ips)
	}
}

func TestResolveMetaOK(t *testing.T) {
	srv := serveFixture(t, "testdata/meta_sample.json")
	dir := t.TempDir()
	r := &Resolver{
		HTTP:      srv.Client(),
		MetaURL:   srv.URL,
		CachePath: filepath.Join(dir, "cache.json"),
		PerNet:    2,
		Seeds:     &Seeds{},
	}
	res := r.Resolve(context.Background())
	if res.Source != "meta" {
		t.Fatalf("source = %s, steps = %+v", res.Source, res.Steps)
	}
	if len(res.IPs) != 9 {
		t.Errorf("ips = %v", res.IPs)
	}
	// 成功后必须刷新缓存
	c := readCacheFile(t, r.CachePath)
	if len(c.IPs) != 9 {
		t.Errorf("cache ips = %v", c.IPs)
	}
}

func TestResolveCascadeDoH(t *testing.T) {
	doh := serveFixture(t, "testdata/doh_sample.json")
	dir := t.TempDir()
	r := &Resolver{
		HTTP:         brokenHTTPClient(),
		MetaURL:      "https://api.github.com/meta",
		DoHEndpoints: []string{doh.URL},
		DoHName:      "github.com",
		CachePath:    filepath.Join(dir, "cache.json"),
		PerNet:       2,
		Seeds:        &Seeds{},
	}
	res := r.Resolve(context.Background())
	if !startsWith(res.Source, "doh:") {
		t.Fatalf("source = %s, steps = %+v", res.Source, res.Steps)
	}
	if !reflect.DeepEqual(res.IPs, []string{"20.205.243.166", "20.201.28.151"}) {
		t.Errorf("ips = %v", res.IPs)
	}
}

func TestResolveViaCache(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "cache.json")
	wantIPs := []string{"140.82.112.1", "140.82.113.1"}
	writeCacheFile(t, cachePath, wantIPs, []string{"*.github.com"})

	r := &Resolver{
		HTTP:         brokenHTTPClient(),
		MetaURL:      "https://api.github.com/meta",
		DoHEndpoints: nil,
		CachePath:    cachePath,
		PerNet:       2,
		Seeds:        &Seeds{},
	}
	res := r.Resolve(context.Background())
	if res.Source != "cache" {
		t.Fatalf("source = %s, steps = %+v", res.Source, res.Steps)
	}
	if !reflect.DeepEqual(res.IPs, wantIPs) {
		t.Errorf("ips = %v", res.IPs)
	}
}

// TestSeedDirect 验证第④级：TLS 直连种子 IP，SNI 与 Host 均为 meta 域名。
func TestSeedDirect(t *testing.T) {
	cert := genCert(t, "api.github.com")
	gotHost := ""
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		data, _ := os.ReadFile("testdata/meta_sample.json")
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	dir := t.TempDir()
	r := &Resolver{
		HTTP:         brokenHTTPClient(), // L1 必败
		MetaURL:      "https://api.github.com/meta",
		DoHEndpoints: nil,
		CachePath:    filepath.Join(dir, "cache.json"),
		Seeds:        &Seeds{SeedIPs: []string{"127.0.0.1", "127.0.0.2"}},
		SeedPort:     portOf(t, srv.URL),
		SeedParallel: 2,
		SeedTimeout:  2 * time.Second,
		TLSRoots:     pool,
		PerNet:       2,
	}
	res := r.Resolve(context.Background())
	if res.Source != "seed-direct" {
		t.Fatalf("source = %s, steps = %+v", res.Source, res.Steps)
	}
	if len(res.IPs) != 9 {
		t.Errorf("ips = %v", res.IPs)
	}
	if gotHost != "api.github.com" {
		t.Errorf("Host 头 = %q, want api.github.com", gotHost)
	}
}

func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := &Resolver{CachePath: filepath.Join(dir, "cache.json")}
	ips := []string{"1.2.3.4", "5.6.7.8"}
	doms := []string{"*.github.com"}
	if err := r.saveCache(ips, doms); err != nil {
		t.Fatal(err)
	}
	gotIPs, gotDoms, err := r.loadCache()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotIPs, ips) || !reflect.DeepEqual(gotDoms, doms) {
		t.Errorf("round trip: %v %v", gotIPs, gotDoms)
	}
}

// --- 测试辅助 ---

func genCert(t *testing.T, dnsName string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: dnsName},
		DNSNames:              []string{dnsName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	cert.Leaf, _ = x509.ParseCertificate(der)
	return cert
}

func portOf(t *testing.T, rawurl string) int {
	t.Helper()
	u, err := url.Parse(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func readCacheFile(t *testing.T, path string) cacheFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c cacheFile
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func writeCacheFile(t *testing.T, path string, ips, doms []string) {
	t.Helper()
	c := cacheFile{SavedAt: time.Now(), IPs: ips, Domains: doms}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func startsWith(s, prefix string) bool {
	return strings.HasPrefix(s, prefix)
}
