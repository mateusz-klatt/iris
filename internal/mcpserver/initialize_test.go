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
