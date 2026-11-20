package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestClientVersionIsObservedOnlyAfterInitializationCompletes(t *testing.T) {
	for _, tc := range []struct {
		name, info, want string
	}{
		{"version", `,"clientInfo":{"name":"codex","version":"0.167.0"}`, "0.167.0"},
		{"empty version", `,"clientInfo":{"name":"codex","version":""}`, ""},
		{"omitted version", `,"clientInfo":{"name":"codex"}`, ""},
		{"missing client info", "", ""},
		{"null client info", `,"clientInfo":null`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			versions := make(chan string, 1)
			serverSide, clientSide := mcp.NewInMemoryTransports()
			server, err := New(nil, WithClientVersion(func(v string) { versions <- v })).Connect(ctx, serverSide, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Close() })
			client, err := clientSide.Connect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			id, err := jsonrpc.MakeID(float64(1))
			if err != nil {
				t.Fatal(err)
			}
			params := json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{}` + tc.info + `}`)
			if err := client.Write(ctx, &jsonrpc.Request{ID: id, Method: "initialize", Params: params}); err != nil {
				t.Fatal(err)
			}
			message, err := client.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			response, ok := message.(*jsonrpc.Response)
			if !ok || response.Error != nil || response.ID != id {
				t.Fatalf("initialize response: %+v", message)
			}
			select {
			case got := <-versions:
				t.Fatalf("version %q observed before initialized notification", got)
			default:
			}
			if err := client.Write(ctx, &jsonrpc.Request{Method: "notifications/initialized", Params: json.RawMessage(`{}`)}); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-versions:
				if got != tc.want {
					t.Fatalf("client version: %q, want %q", got, tc.want)
				}
			case <-ctx.Done():
				t.Fatal("initialized client version was not observed")
			}
		})
	}
}

func TestNilClientVersionCallbackLeavesToolsUsable(t *testing.T) {
	f := &fakeHub{reply: `{}`}
	session := connectHandler(t, f, WithClientVersion(nil))
	out, failed := use(t, session, "whoami", nil)
	if failed || out["execution_id"] != "ex" {
		t.Fatalf("nil observer affected tools: %+v, failed %v", out, failed)
	}
}

// A client of MCP 2026-07-28 has no initialize handshake: it discovers the server and names
// itself in each request's _meta. The meta below is what Codex 0.171.0 sent, captured on a host,
// to the `iris mcp` that served its thread's tools and never saw an initialize.
func TestClientVersionIsObservedFromStatelessRequests(t *testing.T) {
	const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"codex-mcp-client","title":"Codex","version":"0.171.0"},"io.modelcontextprotocol/clientCapabilities":{"experimental":{"codex/auth-change":{}},"extensions":{"codex/connection-lease":{}},"elicitation":{"form":{},"url":{}}}}`
	for _, tc := range []struct {
		name, method, params, want string
	}{
		{"discovery", "server/discover", `{` + meta + `}`, "0.171.0"},
		{"tool list", "tools/list", `{` + meta + `}`, "0.171.0"},
		{"tool call", "tools/call", `{"name":"nope","arguments":{},` + meta + `}`, "0.171.0"},
		{"no client info", "tools/list", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`, ""},
		{"empty version", "tools/list", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"codex-mcp-client","version":""}}}`, ""},
		{"another request", "ping", `{}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			versions := make(chan string, 4)
			serverSide, clientSide := mcp.NewInMemoryTransports()
			server, err := New(nil, WithClientVersion(func(v string) { versions <- v })).Connect(ctx, serverSide, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Close() })
			client, err := clientSide.Connect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			id, err := jsonrpc.MakeID(float64(1))
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Write(ctx, &jsonrpc.Request{ID: id, Method: tc.method, Params: json.RawMessage(tc.params)}); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Read(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-versions:
				if got != tc.want {
					t.Fatalf("client version %q, want %q", got, tc.want)
				}
			default:
				if tc.want != "" {
					t.Fatal("the stateless request's version was not observed")
				}
			}
		})
	}
}
