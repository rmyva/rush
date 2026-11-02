package apiclient

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
)

// ErrRedirect is returned for every redirect response; API credentials are never forwarded.
var (
	ErrRedirect        = errors.New("API redirects are not followed")
	ErrTLSVerification = errors.New("TLS verification failed")
)

type certificatePin struct {
	kind  string
	value [sha256.Size]byte
}

// newTransport creates the hardened transport used by API requests. Proxy is nil so
// bearer credentials cannot leak through HTTP_PROXY/HTTPS_PROXY from the environment.
func newTransport(opts Options) (*http.Transport, error) {
	roots, err := loadRoots(opts.CAFile)
	if err != nil {
		return nil, err
	}
	pin, err := parsePin(opts.Pin)
	if err != nil {
		return nil, err
	}
	config := &tls.Config{ //nolint:gosec // InsecureSkipVerify is restricted to loopback in New.
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{"http/1.1"},
		RootCAs:            roots,
		InsecureSkipVerify: opts.InsecureSkipVerify,
	}
	if pin != nil {
		config.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return fmt.Errorf("%w: %v", ErrTLSVerification, errors.New("TLS peer sent no certificate"))
			}
			leaf := state.PeerCertificates[0]
			var digest [sha256.Size]byte
			if pin.kind == "spki" {
				digest = sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
			} else {
				digest = sha256.Sum256(leaf.Raw)
			}
			if digest != pin.value {
				return fmt.Errorf("%w: certificate pin mismatch", ErrTLSVerification)
			}
			return nil
		}
	}
	if opts.ClientCertFile != "" || opts.ClientKeyFile != "" {
		if opts.ClientCertFile == "" || opts.ClientKeyFile == "" {
			return nil, errors.New("both client certificate and key files are required")
		}
		pair, err := tls.LoadX509KeyPair(opts.ClientCertFile, opts.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client certificate and key: %w", err)
		}
		config.Certificates = []tls.Certificate{pair}
	}
	return &http.Transport{
		Proxy:                 nil,
		DisableKeepAlives:     true,
		DialContext:           (&net.Dialer{Timeout: opts.Timeouts.Dial}).DialContext,
		ForceAttemptHTTP2:     false,
		TLSClientConfig:       config,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSHandshakeTimeout:   opts.Timeouts.TLSHandshake,
		ResponseHeaderTimeout: opts.Timeouts.ResponseHeader,
		IdleConnTimeout:       opts.Timeouts.Idle,
	}, nil
}

func loadRoots(caFile string) (*x509.CertPool, error) {
	if caFile == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA file: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if ok := roots.AppendCertsFromPEM(pem); !ok {
		return nil, errors.New("CA file contains no valid certificates")
	}
	return roots, nil
}

func parsePin(raw string) (*certificatePin, error) {
	if raw == "" {
		return nil, nil
	}
	pin := &certificatePin{}
	switch {
	case strings.HasPrefix(raw, "sha256//"):
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, "sha256//"))
		if err != nil || len(decoded) != sha256.Size {
			return nil, errors.New("invalid public-key pin")
		}
		pin.kind = "spki"
		copy(pin.value[:], decoded)
	case strings.HasPrefix(strings.ToLower(raw), "cert-sha256:"):
		encoded := strings.ReplaceAll(raw[len("cert-sha256:"):], ":", "")
		if len(encoded) != sha256.Size*2 {
			return nil, errors.New("invalid certificate pin")
		}
		decoded, err := hex.DecodeString(encoded)
		if err != nil {
			return nil, errors.New("invalid certificate pin")
		}
		pin.kind = "cert"
		copy(pin.value[:], decoded)
	default:
		return nil, errors.New("invalid TLS pin format")
	}
	return pin, nil
}

func apiHTTPClient(opts Options) (*http.Client, error) {
	transport, err := newTransport(opts)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return ErrRedirect
	}}, nil
}
