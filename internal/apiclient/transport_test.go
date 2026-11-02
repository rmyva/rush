package apiclient

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParsePin(t *testing.T) {
	isolateClientEnv(t)
	want := sha256.Sum256([]byte("pin"))
	for _, raw := range []string{
		"sha256//" + base64.StdEncoding.EncodeToString(want[:]),
		"cert-sha256:" + strings.ToUpper(hex.EncodeToString(want[:])),
		"cert-sha256:" + strings.Join(splitHex(hex.EncodeToString(want[:])), ":"),
	} {
		if _, err := parsePin(raw); err != nil {
			t.Errorf("parsePin(%q): %v", raw, err)
		}
	}
	for _, raw := range []string{"sha256//bad", "sha256//" + base64.RawStdEncoding.EncodeToString(want[:]), "cert-sha256:xyz", "cert-sha256:" + hex.EncodeToString(want[:])[:62], "sha256/"} {
		if _, err := parsePin(raw); err == nil {
			t.Errorf("parsePin(%q) accepted invalid pin", raw)
		}
	}
}

func splitHex(value string) []string {
	parts := make([]string, 0, sha256.Size)
	for i := 0; i < len(value); i += 2 {
		parts = append(parts, value[i:i+2])
	}
	return parts
}

func TestNewValidatesTransportFiles(t *testing.T) {
	isolateClientEnv(t)
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, opts := range []Options{
		{BaseURL: "https://localhost", Token: "x", Pin: "invalid"},
		{BaseURL: "https://localhost", Token: "x", CAFile: filepath.Join(dir, "missing")},
		{BaseURL: "https://localhost", Token: "x", CAFile: ca},
		{BaseURL: "https://localhost", Token: "x", ClientCertFile: "cert"},
		{BaseURL: "https://localhost", Token: "x", ClientKeyFile: "key"},
	} {
		if _, _, err := New(opts); err == nil {
			t.Errorf("New(%+v) accepted invalid transport configuration", opts)
		}
	}
}

func TestTransportTLSSettingsAndRedirect(t *testing.T) {
	isolateClientEnv(t)
	client, err := apiHTTPClient(Options{Timeouts: Timeouts{TLSHandshake: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.ForceAttemptHTTP2 || transport.TLSNextProto == nil || len(transport.TLSNextProto) != 0 {
		t.Fatalf("transport policy: proxySet=%t forceHTTP2=%v TLSNextProto=%v", transport.Proxy != nil, transport.ForceAttemptHTTP2, transport.TLSNextProto)
	}
	config := transport.TLSClientConfig
	if config.MinVersion != tls.VersionTLS13 || len(config.NextProtos) != 1 || config.NextProtos[0] != "http/1.1" {
		t.Fatalf("TLS config: %+v", config)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/next", http.StatusFound) }))
	defer server.Close()
	resp, err := getWithContext(context.Background(), client, server.URL)
	if err == nil || !errorsIsRedirect(err) {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatalf("redirect error = %v, want ErrRedirect", err)
	}
}

func errorsIsRedirect(err error) bool {
	return err == ErrRedirect || strings.Contains(err.Error(), ErrRedirect.Error())
}

func TestTransportHandshakeTimeout(t *testing.T) {
	isolateClientEnv(t)
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	client, _, err := New(Options{
		BaseURL:            "https://" + listener.Addr().String(),
		Token:              "x",
		InsecureSkipVerify: true,
		Timeouts:           Timeouts{TLSHandshake: 100 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.http.CloseIdleConnections()

	requestDone := make(chan error, 1)
	go func() {
		response, requestErr := getWithContext(context.Background(), client.http, client.baseURL.String())
		if response != nil {
			response.Body.Close()
		}
		requestDone <- requestErr
	}()

	select {
	case conn := <-accepted:
		defer conn.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("server did not accept TLS connection")
	}
	select {
	case err := <-requestDone:
		if err == nil || !strings.Contains(err.Error(), "TLS handshake timeout") {
			t.Fatalf("request error = %v, want TLS handshake timeout", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request did not time out during TLS handshake")
	}
	listener.Close()
	select {
	case <-acceptDone:
	case <-time.After(3 * time.Second):
		t.Fatal("accept goroutine did not stop")
	}
}

func TestTransportVerifiesSystemOrProvidedRootsBeforePin(t *testing.T) {
	isolateClientEnv(t)
	cert, key, caPEM := testTLSCertificate(t)
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	spki := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}}
	server.StartTLS()
	defer server.Close()
	addr := server.Listener.Addr().(*net.TCPAddr)
	url := "https://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(addr.Port))
	client, _, err := New(Options{BaseURL: url, Token: "x", CAFile: caFile, Pin: "sha256//" + base64.StdEncoding.EncodeToString(spki[:])})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := getWithContext(ctx, client.http, client.baseURL.String())
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	bad, _, err := New(Options{BaseURL: url, Token: "x", CAFile: caFile, Pin: "cert-sha256:" + strings.Repeat("0", 64)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	badResponse, err := getWithContext(ctx, bad.http, bad.baseURL.String())
	if err == nil {
		if badResponse != nil {
			badResponse.Body.Close()
		}
		t.Fatal("wrong pin accepted")
	}
	untrusted, _, err := New(Options{BaseURL: url, Token: "x", Pin: "sha256//" + base64.StdEncoding.EncodeToString(spki[:])})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	untrustedResponse, err := getWithContext(ctx, untrusted.http, untrusted.baseURL.String())
	if err == nil {
		if untrustedResponse != nil {
			untrustedResponse.Body.Close()
		}
		t.Fatal("pin replaced chain verification")
	}
}

func getWithContext(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(request)
}

func testTLSCertificate(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	key := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)})
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return cert, key, ca
}

func TestTransportTLSMatrix(t *testing.T) {
	isolateClientEnv(t)
	cert, key, caPEM := testTLSCertificate(t)
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	spki := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	leafHash := sha256.Sum256(leaf.Raw)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	start := func(config *tls.Config) *httptest.Server {
		ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
		ts.TLS = config
		ts.StartTLS()
		t.Cleanup(ts.Close)
		return ts
	}
	request := func(opts Options, rawURL string) error {
		opts.BaseURL, opts.Token = rawURL, "test"
		client, _, err := New(opts)
		if err != nil {
			return err
		}
		t.Cleanup(client.http.CloseIdleConnections)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, _, err = client.do(ctx, http.MethodGet, "/", nil, nil, reqOpts{})
		return err
	}
	server := start(&tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}})
	spkiPin := "sha256//" + base64.StdEncoding.EncodeToString(spki[:])
	leafPin := "cert-sha256:" + hex.EncodeToString(leafHash[:])
	if err := request(Options{}, server.URL); err == nil {
		t.Fatal("system roots accepted self-signed certificate")
	}
	if err := request(Options{CAFile: caFile}, server.URL); err != nil {
		t.Fatalf("CA did not trust server: %v", err)
	}
	if err := request(Options{CAFile: caFile}, strings.Replace(server.URL, "127.0.0.1", "localhost", 1)); err == nil {
		t.Fatal("wrong hostname accepted")
	}
	if err := request(Options{CAFile: caFile, Pin: spkiPin}, server.URL); err != nil {
		t.Fatalf("matching SPKI pin rejected: %v", err)
	}
	if err := request(Options{CAFile: caFile, Pin: "sha256//" + base64.StdEncoding.EncodeToString(make([]byte, 32))}, server.URL); err == nil {
		t.Fatal("mismatching SPKI pin accepted")
	}
	if err := request(Options{CAFile: caFile, Pin: leafPin}, server.URL); err != nil {
		t.Fatalf("matching leaf pin rejected: %v", err)
	}
	if err := request(Options{CAFile: caFile, Pin: "cert-sha256:" + strings.Repeat("0", 64)}, server.URL); err == nil {
		t.Fatal("mismatching leaf pin accepted")
	}
	if err := request(Options{Pin: spkiPin}, server.URL); err == nil {
		t.Fatal("pin replaced CA/system verification")
	}

	tls12 := start(&tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12})
	if err := request(Options{CAFile: caFile}, tls12.URL); err == nil {
		t.Fatal("TLS 1.2-only server accepted")
	}

	h2 := httptest.NewUnstartedServer(nil)
	negotiated := make(chan string, 1)
	h2.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocol := ""
		if r.TLS != nil {
			protocol = r.TLS.NegotiatedProtocol
		}
		negotiated <- protocol
		w.WriteHeader(http.StatusNoContent)
	})
	h2.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2", "http/1.1"}}
	h2.StartTLS()
	t.Cleanup(h2.Close)
	if err := request(Options{CAFile: caFile}, h2.URL); err != nil {
		t.Fatalf("HTTP/1.1 with h2 offered failed: %v", err)
	}
	select {
	case protocol := <-negotiated:
		if protocol == "h2" {
			t.Fatal("HTTP/2 negotiated")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("h2 server handler did not run")
	}

	insecure, warnings, err := New(Options{BaseURL: server.URL, Token: "test", InsecureSkipVerify: true, Pin: spkiPin})
	if err != nil {
		t.Fatal(err)
	}
	insecure.http.CloseIdleConnections()
	if len(warnings) != 1 {
		t.Fatalf("loopback insecure warning count = %d", len(warnings))
	}
	if _, _, err := New(Options{BaseURL: "https://example.com", Token: "test", InsecureSkipVerify: true}); err == nil {
		t.Fatal("insecure verification accepted for non-loopback")
	}
}

func TestTransportMutualTLSRequired(t *testing.T) {
	isolateClientEnv(t)
	serverCert, serverKey, caPEM := testTLSCertificate(t)
	ca := x509.NewCertPool()
	if !ca.AppendCertsFromPEM(caPEM) {
		t.Fatal("could not load test CA")
	}
	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	clientTemplate := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "test client"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, clientTemplate, &clientKey.PublicKey, clientKey)
	if err != nil {
		t.Fatal(err)
	}
	clientCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER})
	clientKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(clientKey)})
	clientCert, err := x509.ParseCertificate(clientDER)
	if err != nil {
		t.Fatal(err)
	}
	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(clientCert)
	certFile, keyFile, caFile := filepath.Join(t.TempDir(), "client.pem"), filepath.Join(t.TempDir(), "client-key.pem"), filepath.Join(t.TempDir(), "ca.pem")
	for path, data := range map[string][]byte{certFile: clientCertPEM, keyFile: clientKeyPEM, caFile: caPEM} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	serverPair, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{serverPair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientRoots, MinVersion: tls.VersionTLS13}
	ts.StartTLS()
	defer ts.Close()
	request := func(cert, key string) error {
		client, _, err := New(Options{BaseURL: ts.URL, Token: "test", CAFile: caFile, ClientCertFile: cert, ClientKeyFile: key})
		if err != nil {
			return err
		}
		defer client.http.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, _, err = client.do(ctx, http.MethodGet, "/", nil, nil, reqOpts{})
		return err
	}
	if err := request("", ""); err == nil {
		t.Fatal("mTLS server accepted absent client certificate")
	}
	if err := request(certFile, keyFile); err != nil {
		t.Fatalf("mTLS client certificate rejected: %v", err)
	}
}
