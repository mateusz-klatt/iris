package hub

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/mateusz-klatt/iris/internal/store"
)

// Limits are configured before serving. Zero selects the documented default.
type Limits struct{ TokenPerMinute, AddressPerMinute, WebSockets int }

func RateLimitFlags(flags *flag.FlagSet) *Limits {
	limits := &Limits{}
	flags.IntVar(&limits.TokenPerMinute, "token-rate", 6000, "requests per minute per bearer token")
	flags.IntVar(&limits.AddressPerMinute, "address-rate", 12000, "requests per minute per client IP (forwarded headers ignored)")
	flags.IntVar(&limits.WebSockets, "token-websockets", 100, "concurrent WebSockets per bearer token")
	return limits
}
func (l Limits) Validate() error {
	if l.TokenPerMinute < 0 || l.AddressPerMinute < 0 || l.WebSockets < 0 {
		return fmt.Errorf("rate and WebSocket limits must not be negative")
	}
	return nil
}

type window struct {
	start time.Time
	count int
}

func (h *Hub) allowRate(w http.ResponseWriter, key string, limit int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	bucket := h.rates[key]
	if !now.Before(bucket.start.Add(time.Minute)) {
		bucket = window{start: now}
	}
	// Bound unauthenticated address cardinality. Never evict an active window,
	// because an attacker could then reset its own quota by cycling addresses.
	if bucket.count == 0 && len(h.rates) >= 10000 {
		for k, b := range h.rates {
			if !now.Before(b.start.Add(time.Minute)) {
				delete(h.rates, k)
			}
		}
		if len(h.rates) >= 10000 {
			rateLimited(w, 60)
			return false
		}
	}
	if bucket.count >= limit {
		retry := int(bucket.start.Add(time.Minute).Sub(now).Seconds()) + 1
		rateLimited(w, retry)
		return false
	}
	bucket.count++
	h.rates[key] = bucket
	return true
}
func rateLimited(w http.ResponseWriter, retry int) {
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	writeError(w, &store.Error{Code: "rate_limited", Message: "request quota exceeded; retry later"})
}
func (h *Hub) addressRate(w http.ResponseWriter, r *http.Request) bool {
	address, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		address = r.RemoteAddr
	}
	limit := h.Limits.AddressPerMinute
	if limit == 0 {
		limit = 12000
	}
	return h.allowRate(w, "address:"+address, limit)
}
func (h *Hub) tokenRate(w http.ResponseWriter, p principal) bool {
	if p.name == "" {
		return true
	}
	limit := h.Limits.TokenPerMinute
	if limit == 0 {
		limit = 6000
	}
	return h.allowRate(w, "token:"+p.digest, limit)
}
func (h *Hub) reserveSocket(w http.ResponseWriter, p principal) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	limit := h.Limits.WebSockets
	if limit == 0 {
		limit = 100
	}
	if h.socketCounts[p.digest] >= limit {
		rateLimited(w, 1)
		return false
	}
	h.socketCounts[p.digest]++
	return true
}
func (h *Hub) releaseSocket(p principal) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.socketCounts[p.digest]--
	if h.socketCounts[p.digest] == 0 {
		delete(h.socketCounts, p.digest)
	}
}
