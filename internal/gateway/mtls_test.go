package gateway

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMTLSListener exercises -mtls-ca end to end with an in-test CA (stdlib
// crypto/x509 only): a client certificate signed by the CA is required, and
// the audit fact records the cert's CN as the caller.
func TestMTLSListener(t *testing.T) {
	g, st := newTestGateway(t)
	up := newEchoUpstream(t)
	putRoute(t, st, g, "api", map[string]any{"prefix": "/api/", "upstream": up.srv.URL})

	now := time.Now()
	newTemplate := func(serial int64, cn string) *x509.Certificate {
		return &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    now.Add(-time.Hour),
			NotAfter:     now.Add(time.Hour),
		}
	}

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := newTemplate(1, "test-ca")
	caTmpl.IsCA = true
	caTmpl.KeyUsage = x509.KeyUsageCertSign
	caTmpl.BasicConstraintsValid = true
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	srvKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srvTmpl := newTemplate(2, "gateway")
	srvTmpl.KeyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
	srvTmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	srvTmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	cliKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cliTmpl := newTemplate(3, "ci-client")
	cliTmpl.KeyUsage = x509.KeyUsageDigitalSignature
	cliTmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	cliDER, err := x509.CreateCertificate(rand.Reader, cliTmpl, caCert, &cliKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	writePEM := func(name, typ string, der []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	caPath := writePEM("ca.pem", "CERTIFICATE", caDER)
	certPath := writePEM("srv.pem", "CERTIFICATE", srvDER)
	keyPath := writePEM("srv.key", "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(srvKey))

	cfg, err := MTLSServerConfig(certPath, keyPath, caPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v", cfg.ClientAuth)
	}

	srv := httptest.NewUnstartedServer(g)
	srv.TLS = cfg
	srv.StartTLS()
	t.Cleanup(srv.Close)

	roots := x509.NewCertPool()
	roots.AddCert(caCert)

	// With a CA-signed client cert: proxied, audit key = cert CN.
	withCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: roots,
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{cliDER},
			PrivateKey:  cliKey,
		}},
	}}}
	resp, err := withCert.Get(srv.URL + "/api/x")
	if err != nil {
		t.Fatalf("mTLS client rejected: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("code=%d", resp.StatusCode)
	}
	g.Flush()
	cur := st.Current("gw:api", "gateway")
	if len(cur) != 1 || cur[0].Value["key"] != "ci-client" {
		t.Fatalf("audit key should be the client cert CN: %#v", cur)
	}

	// Without a client cert: the handshake is refused.
	noCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
	if resp, err := noCert.Get(srv.URL + "/api/x"); err == nil {
		resp.Body.Close()
		t.Fatal("connection without a client certificate was accepted")
	}
}

func TestMTLSServerConfigErrors(t *testing.T) {
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MTLSServerConfig(junk, junk, junk); err == nil {
		t.Fatal("bad server keypair accepted")
	}
	if _, err := MTLSServerConfig("", "", filepath.Join(dir, "missing.pem")); err == nil {
		t.Fatal("missing files accepted")
	}
}
