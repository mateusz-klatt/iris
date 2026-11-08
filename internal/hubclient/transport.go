package hubclient

import (
	"net/http"
	"time"
)

// A pooled connection to the hub can die without a word: the hub restarts, a laptop sleeps, a
// NAT forgets the flow. Go's default transport then keeps it, and the next call on it waits for
// the operating system to give up on TCP, which takes minutes. Every hub client therefore pings
// an HTTP/2 connection that has been quiet, drops one that does not answer, and closes idle
// connections soon, so the next call dials afresh.
//
// Pings exist only in HTTP/2, so only a TLS hub gets them. A plain-HTTP/1.1 path, such as a
// loopback hub or a proxy without h2, relies on the idle timeout and the caller's deadline. The
// watch socket is an HTTP/1.1 upgrade, kept alive by the hub's websocket pings, not by these.
var (
	// pingAfter is how long an HTTP/2 connection may receive nothing before it is pinged.
	pingAfter = 15 * time.Second
	// pingTimeout is how long a ping may go unanswered before the connection is closed.
	pingTimeout = 5 * time.Second
	// idleTimeout closes a pooled connection no call has used for that long.
	idleTimeout = 30 * time.Second
)

// newTransport is the transport every hub client uses.
func newTransport() *http.Transport {
	return &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		ForceAttemptHTTP2:   true,
		IdleConnTimeout:     idleTimeout,
		TLSHandshakeTimeout: 10 * time.Second,
		HTTP2:               &http.HTTP2Config{SendPingTimeout: pingAfter, PingTimeout: pingTimeout},
	}
}

// defaultClient replaces http.DefaultClient for every Client without its own HTTP client:
// iris mcp, iris watch and doctor.
var defaultClient = &http.Client{Transport: newTransport()}

// HTTPClient is the shared client with that transport, for code that calls the hub without a
// Client, such as doctor's probes.
func HTTPClient() *http.Client { return defaultClient }
