package wake

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strconv"

	"github.com/coder/websocket"
)

// The proxy is a byte bridge to the daemon selected by the inherited Codex
// environment. It neither starts a daemon nor resumes or subscribes to a thread.
// HTTP and WebSocket framing use the standard libraries, on every platform.
func (c *commands) idle(ctx context.Context, session string) (bool, string) {
	if c.idleRead != nil {
		return c.idleRead(ctx, session)
	}
	ctx, cancel := context.WithTimeout(ctx, c.idleTimeout)
	defer cancel()
	input, send := io.Pipe()
	receive, output := io.Pipe()
	stream := &proxyStream{
		Reader: bufio.NewReader(io.LimitReader(receive, 1<<20)), Writer: send,
		ends: []io.Closer{input, send, receive, output},
	}
	defer func() { _ = stream.Close() }()
	cmd := exec.CommandContext(ctx, c.path, "app-server", "proxy")
	errs := &limitedOutput{limit: 64 << 10}
	cmd.Dir, cmd.Env, cmd.WaitDelay = c.dir, c.env, c.waitDelay
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, output, errs
	if err := cmd.Start(); err != nil {
		return false, "Codex status proxy could not start"
	}
	finished := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		_ = stream.Close()
		finished <- err
	}()
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer func() {
		stop()
		cancel()
		_ = stream.Close()
		<-finished
	}()
	client := &http.Client{Transport: stream}
	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		return false, "Codex status proxy did not establish its connection"
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(256 << 10)
	status, err := readIdle(ctx, conn, session)
	if err != nil {
		return false, "Codex thread status could not be verified"
	}
	// The daemon proxy does not consistently acknowledge a WebSocket close.
	// EOF ends only our bridge; admission needs no peer close-handshake receipt.
	_ = conn.CloseNow()
	select {
	case err := <-finished:
		// Keep the deferred reap single-use after observing the process exit.
		finished <- err
		if (err != nil && !errors.Is(err, io.ErrClosedPipe)) || ctx.Err() != nil || errs.overflow {
			return false, "Codex status proxy exceeded its bound or failed"
		}
	case <-ctx.Done():
		return false, "Codex status proxy exceeded its bound or failed"
	}
	if status == "active" {
		return false, "Codex thread is active"
	}
	return true, ""
}

// proxyStream retains bytes buffered past the HTTP upgrade. Closing it also
// unblocks exec's copying goroutines, including when the child keeps a pipe open.
type proxyStream struct {
	*bufio.Reader
	io.Writer
	ends []io.Closer
}

func (p *proxyStream) Close() error {
	for _, end := range p.ends {
		_ = end.Close()
	}
	return nil
}

func (p *proxyStream) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := request.Write(p.Writer); err != nil {
		return nil, err
	}
	response, err := http.ReadResponse(p.Reader, request)
	if err != nil {
		return nil, err
	}
	response.Body = p
	return response, nil
}

// Only a correlated reply about the exact loaded root can authorize admission.
// Unrelated notifications carry no decision and are bounded, never persisted.
func readIdle(ctx context.Context, conn *websocket.Conn, session string) (string, error) {
	_, err := proxyCall(ctx, conn, 1, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "iris-wake", "version": "1"},
	})
	if err == nil {
		err = conn.Write(ctx, websocket.MessageText, []byte(`{"method":"initialized","params":{}}`))
	}
	if err != nil {
		return "", err
	}
	result, err := proxyCall(ctx, conn, 2, "thread/read", map[string]any{"threadId": session, "includeTurns": false})
	if err != nil {
		return "", err
	}
	var reply struct {
		Thread struct {
			ID     string `json:"id"`
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &reply); err != nil {
		return "", err
	}
	if reply.Thread.ID != session || (reply.Thread.Status.Type != "idle" && reply.Thread.Status.Type != "active") {
		return "", errors.New("thread status is unavailable")
	}
	return reply.Thread.Status.Type, nil
}

func proxyCall(ctx context.Context, conn *websocket.Conn, id int, method string, params map[string]any) (json.RawMessage, error) {
	// All parameters are fixed scalar values; encoding cannot fail.
	request, _ := json.Marshal(struct {
		ID     int            `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}{id, method, params})
	if err := conn.Write(ctx, websocket.MessageText, request); err != nil {
		return nil, err
	}
	for range 32 {
		kind, payload, err := conn.Read(ctx)
		if err != nil {
			return nil, err
		}
		var reply struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if kind != websocket.MessageText || json.Unmarshal(payload, &reply) != nil {
			return nil, errors.New("invalid proxy response")
		}
		if string(reply.ID) != strconv.Itoa(id) {
			continue
		}
		if len(reply.Result) == 0 || (len(reply.Error) != 0 && string(reply.Error) != "null") {
			return nil, errors.New("proxy request failed")
		}
		return reply.Result, nil
	}
	return nil, errors.New("proxy response count exceeded its bound")
}
