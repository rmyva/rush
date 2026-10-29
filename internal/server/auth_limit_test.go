package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

type countedBody struct {
	*bytes.Reader
	read int
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func TestAuthRejectsOversizedBodyWithoutDrainingIt(t *testing.T) {
	a := newAuth()
	bodyBytes := append([]byte(`{"token":"`+a.token+`"}`), bytes.Repeat([]byte(" "), 1<<20)...)
	body := &countedBody{Reader: bytes.NewReader(bodyBytes)}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auth", body)
	w := httptest.NewRecorder()
	a.HandleAuth(w, r)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	require.Less(t, body.read, len(bodyBytes), "handler must reject without reading the full body")
}

func TestAuthLoginStillWorks(t *testing.T) {
	a := newAuth()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auth", bytes.NewBufferString(`{"token":"`+a.token+`"}`))
	w := httptest.NewRecorder()
	a.HandleAuth(w, r)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotEmpty(t, w.Result().Cookies())
}

func TestAuthBodyBoundaryAndTrailingData(t *testing.T) {
	t.Parallel()
	a := newAuth()
	valid := []byte(`{"token":"` + a.token + `"}`)
	for _, tc := range []struct {
		name   string
		body   []byte
		status int
	}{
		{"exact limit", append(bytes.Clone(valid), bytes.Repeat([]byte(" "), maxAuthBodyBytes-len(valid))...), http.StatusOK},
		{"one byte over", append(bytes.Clone(valid), bytes.Repeat([]byte(" "), maxAuthBodyBytes-len(valid)+1)...), http.StatusRequestEntityTooLarge},
		{"second JSON value", append(bytes.Clone(valid), []byte(` {}`)...), http.StatusBadRequest},
		{"invalid trailing data", append(bytes.Clone(valid), []byte(` garbage`)...), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auth", bytes.NewReader(tc.body))
			w := httptest.NewRecorder()
			a.HandleAuth(w, r)
			require.Equal(t, tc.status, w.Code)
			if tc.status != http.StatusOK {
				require.Empty(t, w.Result().Cookies())
			}
		})
	}
}
