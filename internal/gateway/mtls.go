package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// MTLSServerConfig builds the proxy listener's TLS config for -mtls-ca:
// serve certFile/keyFile, and require + verify a client certificate signed by
// (a CA in) caFile on every connection. Audit facts then record the client
// cert's CN as the caller (unless key/OIDC auth already named one).
func MTLSServerConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: load server cert: %w", err)
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("mtls: no certificates in %s", caFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
