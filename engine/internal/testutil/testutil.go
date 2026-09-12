// Package testutil 提供跨包共享的测试设施：自签证书与假 HTTPS 上游。
// 仅供测试使用，不进入任何产品路径。
package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"time"
)

// SelfSignedCert 生成 dnsName 的自签证书（已填 Leaf），供假上游使用；
// 客户端用 RootCAs 池加 cert.Leaf 即可完成信任。
func SelfSignedCert(dnsName string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: dnsName},
		DNSNames:              []string{dnsName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	c := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	if c.Leaf, err = x509.ParseCertificate(der); err != nil {
		return tls.Certificate{}, err
	}
	return c, nil
}

// TLSServer 是一个响应 200 OK 的本地假 HTTPS 上游。
type TLSServer struct {
	Addr string
	Cert tls.Certificate

	ln  net.Listener
	srv *http.Server
}

// StartTLSServer 在 127.0.0.1 随机端口起假上游（dnsName 自签），
// 任何路径返回 "200 OK"。stop 关闭服务。
func StartTLSServer(dnsName string, handler http.HandlerFunc) (*TLSServer, func(), error) {
	cert, err := SelfSignedCert(dnsName)
	if err != nil {
		return nil, nil, fmt.Errorf("自签证书: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("监听: %w", err)
	}
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "OK")
		}
	}
	s := &TLSServer{
		Addr: ln.Addr().String(),
		Cert: cert,
		ln:   ln,
		srv: &http.Server{
			Handler:   handler,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
		},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.srv.ServeTLS(ln, "", "")
	}()
	stop := func() {
		_ = s.srv.Close()
		<-done
	}
	return s, stop, nil
}

// ClientTLSConfig 返回信任该假上游证书、SNI 为 dnsName 的客户端配置。
func (s *TLSServer) ClientTLSConfig(dnsName string) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(s.Cert.Leaf)
	return &tls.Config{ServerName: dnsName, RootCAs: pool}
}
