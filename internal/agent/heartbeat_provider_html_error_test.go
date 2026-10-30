package agent

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/agent/codexprovider"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestIsHTMLProviderError(t *testing.T) {
	tests := []struct {
		name string
		err  *fantasy.ProviderError
		want bool
	}{
		{name: "doctype in body", err: &fantasy.ProviderError{ResponseBody: []byte(" <!DOCTYPE html><html>blocked</html>")}, want: true},
		{name: "doctype case and whitespace", err: &fantasy.ProviderError{Message: " \n<!DoCtYpE HTML><html>blocked</html>"}, want: true},
		{name: "html in message", err: &fantasy.ProviderError{Message: "<HTML><body>blocked</body></HTML>"}, want: true},
		{name: "response body fallback", err: &fantasy.ProviderError{ResponseBody: []byte("<html><title>Blocked</title></html>")}, want: true},
		{name: "bounded diagnostic", err: &fantasy.ProviderError{Message: "<html><body>" + strings.Repeat("é", 500) + "</body></html>"}, want: true},
		{name: "message takes precedence over response body", err: &fantasy.ProviderError{Message: "temporarily unavailable", ResponseBody: []byte("<html>not diagnostic</html>")}},
		{name: "embedded html is not page", err: &fantasy.ProviderError{ResponseBody: []byte(`{"html":"<html>not an error page</html>"}`)}},
		{name: "content type alone is insufficient", err: &fantasy.ProviderError{ResponseHeaders: map[string]string{"Content-Type": "text/html"}}},
		{name: "ordinary json", err: &fantasy.ProviderError{ResponseBody: []byte(`{"error":"unavailable"}`)}},
		{name: "html substring is not page", err: &fantasy.ProviderError{Message: "server returned html"}},
		{name: "empty", err: &fantasy.ProviderError{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isHTMLProviderError(tt.err))
			if tt.want {
				diagnostic, ok := sanitizeProviderHTML(tt.err)
				require.True(t, ok)
				require.LessOrEqual(t, len(diagnostic), 262)
			}
		})
	}
}

func TestSanitizeProviderHTML(t *testing.T) {
	tests := []struct {
		name string
		err  *fantasy.ProviderError
		want string
	}{
		{name: "style and title", err: &fantasy.ProviderError{StatusCode: 403, Message: `<html><style>secret style</style><title>Denied</title><body>secret body</body></html>`}, want: "HTTP 403 error page: Denied"},
		{name: "no title", err: &fantasy.ProviderError{StatusCode: 403, Message: `<html><style>secret style</style><body>Visible detail</body></html>`}, want: "HTTP 403 error page: Visible detail"},
		{name: "json", err: &fantasy.ProviderError{StatusCode: 403, Message: `{"error":"no"}`}},
		{name: "empty", err: &fantasy.ProviderError{StatusCode: 403}},
		{name: "long", err: &fantasy.ProviderError{StatusCode: 403, Message: `<html><body>` + strings.Repeat("x", 300) + `</body></html>`}, want: "long"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := sanitizeProviderHTML(tt.err)
			if tt.want == "" {
				require.False(t, ok)
				return
			}
			require.True(t, ok)
			if tt.name == "long" {
				require.LessOrEqual(t, len(got), len("HTTP 403 error page: ")+240+len("…"))
				require.NotContains(t, got, strings.Repeat("x", 241))
				return
			}
			require.Equal(t, tt.want, got)
		})
	}
}

func TestHeartbeatProviderWrapsHTMLProviderError(t *testing.T) {
	sentinel := errors.New("upstream sentinel")
	original := &fantasy.ProviderError{
		Title:      "bad gateway",
		Message:    "<!doctype html><html><title>Gateway Error</title><body>secret gateway details</body></html>",
		StatusCode: http.StatusBadGateway,
		Cause:      sentinel,
	}
	wrapped := fmt.Errorf("SDK wrapper: %w", original)
	inner := &hbFakeModel{provider: "test", model: "model", genErr: wrapped}
	_, err := (&hbLanguageModel{inner: inner}).Generate(t.Context(), fantasy.Call{})
	require.Error(t, err)
	var providerErr *fantasy.ProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Same(t, original, providerErr)
	require.Equal(t, http.StatusBadGateway, providerErr.StatusCode)
	require.Contains(t, err.Error(), "SDK wrapper:")
	require.Contains(t, err.Error(), "Gateway Error")
	require.LessOrEqual(t, len(err.Error()), 320)
	require.NotContains(t, err.Error(), "secret gateway details")
	require.ErrorIs(t, err, sentinel)
	require.ErrorIs(t, err, original)
}

func TestHeartbeatProviderWrapsRealProviderHTMLResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<!doctype html><html><title>Gateway Error</title><body>secret gateway details</body></html>"))
	}))
	t.Cleanup(server.Close)
	provider, err := openaicompat.New(openaicompat.WithBaseURL(server.URL), openaicompat.WithAPIKey("test"))
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "model")
	require.NoError(t, err)

	_, err = (&hbLanguageModel{inner: model}).Generate(t.Context(), fantasy.Call{})
	require.Error(t, err)
	var providerErr *fantasy.ProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, http.StatusBadGateway, providerErr.StatusCode)
	require.Contains(t, err.Error(), "Gateway Error")
	require.NotContains(t, err.Error(), "secret gateway details")
}

func TestHeartbeatProviderWrapsRealCodex403HTMLResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<!doctype html><html><title>Access denied</title><body>secret Codex policy detail</body></html>"))
	}))
	t.Cleanup(server.Close)
	client := &http.Client{Transport: codexHTTPTestTransport{server: server}}
	provider, err := codexprovider.New(client, &oauth.Token{AccessToken: "test", AccountID: "test-account"})
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)

	_, err = (&hbLanguageModel{inner: model}).Generate(t.Context(), fantasy.Call{})
	require.Error(t, err)
	var providerErr *fantasy.ProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, http.StatusForbidden, providerErr.StatusCode)
	require.Contains(t, err.Error(), "Access denied")
	require.Contains(t, err.Error(), "HTTP 403")
	require.NotContains(t, err.Error(), "secret Codex policy detail")
}

type codexHTTPTestTransport struct{ server *httptest.Server }

func (rt codexHTTPTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	forwarded := req.Clone(req.Context())
	serverURL, err := http.NewRequestWithContext(req.Context(), http.MethodGet, rt.server.URL, nil)
	if err != nil {
		return nil, err
	}
	forwarded.URL.Scheme = serverURL.URL.Scheme
	forwarded.URL.Host = serverURL.URL.Host
	forwarded.Host = serverURL.URL.Host
	return rt.server.Client().Transport.RoundTrip(forwarded)
}

func TestHeartbeatProviderWrapsHTMLStreamError(t *testing.T) {
	original := &fantasy.ProviderError{StatusCode: http.StatusServiceUnavailable, Message: "<html>secret stream gateway</html>"}
	inner := &hbFakeModel{provider: "test", model: "model", streamErr: original}
	_, err := (&hbLanguageModel{inner: inner}).Stream(t.Context(), fantasy.Call{})
	require.Error(t, err)
	var providerErr *fantasy.ProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, http.StatusServiceUnavailable, providerErr.StatusCode)
	require.Contains(t, err.Error(), "secret stream gateway")
	require.NotContains(t, err.Error(), "<html>")
	require.ErrorIs(t, err, original)
}

func TestHeartbeatProviderSanitizesHTMLStreamErrorPart(t *testing.T) {
	sentinel := errors.New("stream sentinel")
	original := &fantasy.ProviderError{
		Title:      "access denied",
		Message:    `<!doctype html><html><style>body{display:none}.secret-cloudflare-css{color:red}</style><title>Attention Required! | Cloudflare</title><body>secret Cloudflare challenge details</body></html>`,
		StatusCode: http.StatusForbidden,
		Cause:      sentinel,
	}
	wrapped := fmt.Errorf("SDK wrapper: %w", original)
	other := fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "before"}
	inner := &hbFakeModel{provider: "test", model: "model", parts: []fantasy.StreamPart{
		other,
		{Type: fantasy.StreamPartTypeError, Error: wrapped},
	}}
	stream, err := (&hbLanguageModel{inner: inner}).Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	var got []fantasy.StreamPart
	for part := range stream {
		got = append(got, part)
	}
	require.Len(t, got, 2)
	require.Equal(t, other, got[0])
	require.Equal(t, fantasy.StreamPartTypeError, got[1].Type)
	require.Contains(t, got[1].Error.Error(), "SDK wrapper:")
	require.Contains(t, got[1].Error.Error(), "Attention Required! | Cloudflare")
	require.Contains(t, got[1].Error.Error(), "HTTP 403")
	require.LessOrEqual(t, len(got[1].Error.Error()), 320)
	require.NotContains(t, got[1].Error.Error(), "secret Cloudflare challenge details")
	require.NotContains(t, got[1].Error.Error(), "<html")
	require.NotContains(t, got[1].Error.Error(), "<style")
	var providerErr *fantasy.ProviderError
	require.ErrorAs(t, got[1].Error, &providerErr)
	require.Same(t, original, providerErr)
	require.Equal(t, http.StatusForbidden, providerErr.StatusCode)
	require.ErrorIs(t, got[1].Error, original)
	require.ErrorIs(t, got[1].Error, sentinel)
}

func TestHeartbeatProviderSanitizesRealCodexLazyStream403HTML(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<!doctype html><html><style>body{display:none}.secret-cloudflare-css{color:red}</style><title>Attention Required! | Cloudflare</title><body>secret Cloudflare challenge details</body></html>`))
	}))
	t.Cleanup(server.Close)
	client := &http.Client{Transport: codexHTTPTestTransport{server: server}}
	provider, err := codexprovider.New(client, &oauth.Token{AccessToken: "test", AccountID: "test-account"})
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)

	stream, err := (&hbLanguageModel{inner: model}).Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err, "Codex Stream returns a lazy iterator")
	var got []fantasy.StreamPart
	for part := range stream {
		got = append(got, part)
	}
	require.Len(t, got, 1)
	require.Equal(t, fantasy.StreamPartTypeError, got[0].Type)
	var providerErr *fantasy.ProviderError
	require.ErrorAs(t, got[0].Error, &providerErr)
	require.Equal(t, http.StatusForbidden, providerErr.StatusCode)
	require.Contains(t, got[0].Error.Error(), "Attention Required! | Cloudflare")
	require.Contains(t, got[0].Error.Error(), "HTTP 403")
	require.LessOrEqual(t, len(got[0].Error.Error()), 320)
	require.NotContains(t, got[0].Error.Error(), "secret Cloudflare challenge details")
	require.NotContains(t, got[0].Error.Error(), "<html")
	require.NotContains(t, got[0].Error.Error(), "<style")
}

func TestWrapProviderHTTPErrorPreservesNonHTMLError(t *testing.T) {
	original := &fantasy.ProviderError{StatusCode: 503, Message: "temporarily unavailable"}
	require.Same(t, original, wrapProviderHTTPError(original))
}

func TestHeartbeatProviderSanitizesHTMLObjectStreamErrorPart(t *testing.T) {
	sentinel := errors.New("object stream sentinel")
	original := &fantasy.ProviderError{
		StatusCode: http.StatusForbidden,
		Message:    "<html><title>Access denied</title><style>hidden CSS</style></html>",
		Cause:      sentinel,
	}
	source := func(yield func(fantasy.ObjectStreamPart) bool) {
		yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeError, Error: original})
	}
	var parts []fantasy.ObjectStreamPart
	for part := range observeObjectStream(t.Context(), "test", "model", source) {
		parts = append(parts, part)
	}
	require.Len(t, parts, 1)
	require.Equal(t, fantasy.ObjectStreamPartTypeError, parts[0].Type)
	require.Contains(t, parts[0].Error.Error(), "HTTP 403")
	require.Contains(t, parts[0].Error.Error(), "Access denied")
	require.NotContains(t, parts[0].Error.Error(), "<html>")
	require.NotContains(t, parts[0].Error.Error(), "hidden CSS")
	var providerErr *fantasy.ProviderError
	require.ErrorAs(t, parts[0].Error, &providerErr)
	require.Same(t, original, providerErr)
	require.ErrorIs(t, parts[0].Error, sentinel)
}
