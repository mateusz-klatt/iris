package hubclient

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// silentProxy forwards TCP to target until silence is called; from then on, connections already
// open stay open but carry nothing either way, as one to a hub that restarted, or across a NAT
// that forgot the flow, does. New connections still work.
type silentProxy struct {
	ln     net.Listener
	mu     sync.Mutex
	conns  []*atomic.Bool
	dialed atomic.Int32
}

func newSilentProxy(t *testing.T, target string) *silentProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &silentProxy{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.dialed.Add(1)
			quiet := &atomic.Bool{}
			p.mu.Lock()
			p.conns = append(p.conns, quiet)
			p.mu.Unlock()
			pipe := func(dst, src net.Conn) {
				buf := make([]byte, 32<<10)
				for {
					n, err := src.Read(buf)
					if n > 0 && !quiet.Load() {
						if _, werr := dst.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}
			go pipe(up, c)
			go pipe(c, up)
			t.Cleanup(func() { _ = c.Close(); _ = up.Close() })
		}
	}()
	return p
}

func (p *silentProxy) silence() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, q := range p.conns {
		q.Store(true)
	}
}

// A pooled HTTP/2 connection that goes silent is found by its ping and dropped, so the next call
// dials afresh and succeeds within a bound. Go's default transport never pings and keeps such a
// connection for 90 s, so the call waited on it for as long as TCP took to give up.
func TestASilentPooledConnectionIsDroppedAndTheNextCallSucceeds(t *testing.T) {
	oldAfter, oldTimeout := pingAfter, pingTimeout
	pingAfter, pingTimeout = 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { pingAfter, pingTimeout = oldAfter, oldTimeout })

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	proxy := newSilentProxy(t, srv.Listener.Addr().String())

	tr := newTransport()
	tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	client := &http.Client{Transport: tr}
	defer tr.CloseIdleConnections()
	url := "https://" + proxy.ln.Addr().String() + "/"
	get := func(bound time.Duration) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), bound)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		res, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer func() { _ = res.Body.Close() }()
		body, err := io.ReadAll(res.Body)
		return string(body), err
	}
	if proto, err := get(5 * time.Second); err != nil || proto != "HTTP/2.0" {
		t.Fatalf("first call: %q %v", proto, err)
	}
	proxy.silence()
	// While the pooled connection is idle, its ping goes unanswered and it is closed.
	time.Sleep(pingAfter + pingTimeout + 300*time.Millisecond)
	if proto, err := get(2 * time.Second); err != nil || proto != "HTTP/2.0" {
		t.Fatalf("the call after the connection went silent: %q %v", proto, err)
	}
	if n := proxy.dialed.Load(); n != 2 {
		t.Fatalf("%d connections, want a fresh second one", n)
	}
}

// Every hub client without its own HTTP client gets that transport, and so does the hedge.
func TestEveryHubClientPingsAndDropsIdleConnections(t *testing.T) {
	for name, rt := range map[string]http.RoundTripper{"default": NoRedirects(nil).Transport, "hedge primary": HedgedClient().Transport.(hedge).primary, "hedge backup": HedgedClient().Transport.(hedge).backup} {
		tr, ok := rt.(*http.Transport)
		if !ok || tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout != pingAfter || tr.HTTP2.PingTimeout != pingTimeout || tr.IdleConnTimeout != idleTimeout {
			t.Fatalf("%s: %+v", name, rt)
		}
	}
}
