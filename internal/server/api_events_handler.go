package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

const apiSSEStreamsPerToken = 8

type apiEventsSessionStore interface {
	GetSession(*http.Request, string) (session.Session, error)
	OwnsSession(session.Session) bool
}

type apiSSETicker interface {
	C() <-chan time.Time
	Stop()
}

type apiSSETickerFactory func() apiSSETicker

type apiSSELimiter struct {
	mu     sync.Mutex
	counts map[string]int
}

type apiEventsHandler struct {
	store   apiEventsSessionStore
	log     *APIEventLog
	limiter *apiSSELimiter
	ticker  apiSSETickerFactory
	gate    session.FinalizationTerminalGate
}

func newAPIEventsHandler(store apiEventsSessionStore, log *APIEventLog, limiter *apiSSELimiter, ticker apiSSETickerFactory, gates ...session.FinalizationTerminalGate) http.HandlerFunc {
	if limiter == nil {
		limiter = &apiSSELimiter{counts: make(map[string]int)}
	}
	if ticker == nil {
		ticker = func() apiSSETicker { return apiRealSSETicker{ticker: time.NewTicker(15 * time.Second)} }
	}
	h := apiEventsHandler{store: store, log: log, limiter: limiter, ticker: ticker}
	// Fork patch (finalization): an explicit gate (test seam) wins;
	// otherwise the production store derives it from its app, nil with the
	// flag off.
	if len(gates) > 0 {
		h.gate = gates[0]
	} else if ws, ok := store.(apiAppWriteStore); ok {
		h.gate = finalizationTerminalGateFor(ws.app)
	}
	return h.serveHTTP
}

type apiRealSSETicker struct{ ticker *time.Ticker }

func (t apiRealSSETicker) C() <-chan time.Time { return t.ticker.C }
func (t apiRealSSETicker) Stop()               { t.ticker.Stop() }

func (h apiEventsHandler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	item, err := h.store.GetSession(r, r.PathValue("id"))
	if err != nil || !h.store.OwnsSession(item) {
		apiNotFound(w, r, "session")
		return
	}
	after, present, err := apiSSELastID(r)
	if err != nil {
		WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid Last-Event-ID", "", nil)
		return
	}
	principal, _ := PrincipalFrom(r.Context())
	if !h.limiter.acquire(principal.TokenID) {
		w.Header().Set("Retry-After", "5")
		WriteAPIProblem(w, r, http.StatusTooManyRequests, "Too Many Requests", "too many event streams", "", nil)
		return
	}
	defer h.limiter.release(principal.TokenID)
	ch, overflow, unsubscribe := h.log.Subscribe(item.ID)
	defer unsubscribe()
	// Fork patch (finalization): nil gate (flag off) returns ch unchanged;
	// a held terminal "run" event waits for the owner's seal. NOTE: the
	// Last-Event-ID replay below bypasses this gate (known gap, out of
	// scope).
	ch = FinalizationGatedAPIEventStream(r.Context(), h.gate, item.ID, ch)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return
	}
	flush := func() error { return http.NewResponseController(w).Flush() }
	if err := flush(); err != nil {
		return
	}
	last := uint64(0)
	if present {
		events, oldest, retained := h.log.Replay(item.ID, after)
		if retained && oldest != 0 && after < oldest-1 {
			if err := apiSSEWrite(w, APIEvent{Type: "reset", Data: json.RawMessage(`{"reason":"history_truncated"}`)}); err != nil {
				return
			}
			if err := flush(); err != nil {
				return
			}
		}
		last = after
		for _, event := range events {
			if event.ID <= last {
				continue
			}
			if err := apiSSEWrite(w, event); err != nil {
				return
			}
			last = event.ID
		}
		if err := flush(); err != nil {
			return
		}
	}
	replayed := last
	ticker := h.ticker()
	defer ticker.Stop()
	revoked := apiSSERevocation(r.Context(), principal)
	for {
		select {
		case <-revoked:
			_ = apiSSEWrite(w, APIEvent{Type: "closed", Data: json.RawMessage(`{"reason":"token_revoked"}`)})
			_ = flush()
			return
		case <-r.Context().Done():
			return
		case <-overflow:
			_ = apiSSEWrite(w, APIEvent{Type: "overflow", Data: json.RawMessage(`{}`)})
			_ = flush()
			return
		case event, ok := <-ch:
			if !ok {
				_ = apiSSEWrite(w, APIEvent{Type: "closed", Data: json.RawMessage(`{"reason":"session_deleted"}`)})
				_ = flush()
				return
			}
			// Live events dedupe only against the replay boundary: a held
			// terminal event legitimately arrives after higher event ids,
			// so a running high-water mark would drop it forever.
			if event.ID <= replayed {
				continue
			}
			if err := apiSSEWrite(w, event); err != nil {
				return
			}
			if err := flush(); err != nil {
				return
			}
		case <-ticker.C():
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			if err := flush(); err != nil {
				return
			}
		}
	}
}

func (l *apiSSELimiter) acquire(token string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts == nil {
		l.counts = make(map[string]int)
	}
	if l.counts[token] >= apiSSEStreamsPerToken {
		return false
	}
	l.counts[token]++
	return true
}

func (l *apiSSELimiter) release(token string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts[token] <= 1 {
		delete(l.counts, token)
		return
	}
	l.counts[token]--
}

func apiSSELastID(r *http.Request) (uint64, bool, error) {
	value := r.Header.Get("Last-Event-ID")
	if value == "" {
		value = r.URL.Query().Get("last_event_id")
	}
	if value == "" {
		return 0, false, nil
	}
	id, err := strconv.ParseUint(value, 10, 64)
	if err != nil || id == math.MaxUint64 && errors.Is(err, strconv.ErrRange) {
		return 0, true, errors.New("invalid event id")
	}
	return id, true, nil
}

func apiSSEWrite(w http.ResponseWriter, event APIEvent) error {
	data := event.Data
	if !json.Valid(data) {
		data = json.RawMessage(`null`)
	}
	_, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, event.Type, data)
	return err
}
