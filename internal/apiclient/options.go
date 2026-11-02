package apiclient

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultMaxResponseBytes int64 = 8 << 20
	maxResponseBytesLimit   int64 = 1 << 30
)

type Timeouts struct {
	Dial           time.Duration
	TLSHandshake   time.Duration
	ResponseHeader time.Duration
	Idle           time.Duration
}

type Options struct {
	BaseURL            string
	Token              string
	CAFile             string
	Pin                string
	ClientCertFile     string
	ClientKeyFile      string
	AllowCleartext     bool
	InsecureSkipVerify bool
	UserAgent          string
	Timeouts           Timeouts
	MaxResponseBytes   int64
	Sleeper            func(context.Context, time.Duration) error
	Rand               io.Reader
	Now                func() time.Time
}

// Client holds the validated immutable configuration used by subsequent request methods.
type Client struct {
	options Options
	baseURL *url.URL
	http    *http.Client
}

// New validates options and returns the normalized configuration for a client.
func New(opts Options) (*Client, []string, error) {
	if opts.Token == "" {
		return nil, nil, &UsageError{Err: errors.New("API token is required")}
	}
	if opts.MaxResponseBytes < 0 || opts.MaxResponseBytes > maxResponseBytesLimit {
		return nil, nil, &UsageError{Err: errors.New("max response bytes must be between 0 and 1 GiB")}
	}
	baseURL, loopback, err := validateBaseURL(opts.BaseURL, opts.AllowCleartext)
	if err != nil {
		return nil, nil, &UsageError{Err: err}
	}
	if opts.InsecureSkipVerify && !loopback {
		return nil, nil, &UsageError{Err: errors.New("insecure skip verify is allowed only for loopback URLs")}
	}
	if opts.Timeouts.Dial == 0 {
		opts.Timeouts.Dial = 10 * time.Second
	}
	if opts.Timeouts.TLSHandshake == 0 {
		opts.Timeouts.TLSHandshake = 10 * time.Second
	}
	if opts.Timeouts.ResponseHeader == 0 {
		opts.Timeouts.ResponseHeader = 30 * time.Second
	}
	if opts.Timeouts.Idle == 0 {
		opts.Timeouts.Idle = 90 * time.Second
	}
	if opts.MaxResponseBytes == 0 {
		opts.MaxResponseBytes = defaultMaxResponseBytes
	}
	if opts.Sleeper == nil {
		opts.Sleeper = defaultSleeper
	}
	if opts.Rand == nil {
		opts.Rand = rand.Reader
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	httpClient, err := apiHTTPClient(opts)
	if err != nil {
		return nil, nil, &UsageError{Err: err}
	}
	client := &Client{options: opts, baseURL: baseURL, http: httpClient}
	var warnings []string
	if opts.InsecureSkipVerify {
		warnings = append(warnings, "TLS certificate verification is disabled for a loopback URL")
	}
	if opts.AllowCleartext && baseURL.Scheme == "http" && !loopback {
		warnings = append(warnings, "cleartext HTTP is enabled for a non-loopback URL")
	}
	return client, warnings, nil
}

func validateBaseURL(raw string, allowCleartext bool) (*url.URL, bool, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, false, errors.New("invalid API base URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, false, errors.New("API base URL must use https or http")
	}
	if u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return nil, false, errors.New("API base URL must include a host")
	}
	if u.User != nil {
		return nil, false, errors.New("API base URL must not contain userinfo")
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, false, errors.New("API base URL must not contain a fragment")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, false, errors.New("invalid API base URL query")
	}
	for key := range query {
		if strings.EqualFold(key, "token") || strings.EqualFold(key, "access_token") {
			return nil, false, errors.New("API base URL must not contain a token query parameter")
		}
	}
	loopback := isLoopbackHost(u.Hostname())
	if u.Scheme == "http" && !loopback && !allowCleartext {
		return nil, false, errors.New("cleartext HTTP requires a loopback host or AllowCleartext")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if u.Path == "" {
		u.Path = "/"
	}
	u.RawPath = ""
	return u, loopback, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func defaultSleeper(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
