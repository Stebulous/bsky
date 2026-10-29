package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bluesky-social/indigo/xrpc"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func TestDMPreviewAndReadsDoNotWrite(t *testing.T) {
	var writes, reads int
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Atproto-Proxy") != "did:web:api.bsky.chat#bsky_chat" {
			t.Error("missing proxy header")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			writes++
		} else {
			reads++
		}
		switch strings.TrimPrefix(r.URL.Path, "/xrpc/") {
		case "chat.bsky.convo.listConvos":
			if r.URL.Query().Get("limit") != "20" || r.URL.Query().Get("kind") != "direct" {
				t.Error(r.URL.RawQuery)
			}
			json.NewEncoder(w).Encode(map[string]any{"convos": []any{}, "cursor": "next"})
		case "chat.bsky.convo.getMessages":
			json.NewEncoder(w).Encode(map[string]any{"messages": []any{}, "cursor": "older"})
		case "chat.bsky.convo.getConvo", "chat.bsky.convo.updateRead":
			json.NewEncoder(w).Encode(map[string]any{"convo": map[string]any{"id": "one", "members": []any{}, "rev": "1", "muted": false, "unreadCount": 1}})
		case "chat.bsky.convo.sendMessage":
			json.NewEncoder(w).Encode(map[string]any{"id": "new", "rev": "2", "text": "approved", "sender": map[string]any{"did": testDid}, "sentAt": "2026-10-29T00:00:00Z"})
		default:
			t.Error(r.URL.Path)
			http.Error(w, "unknown", 404)
		}
	}))
	defer fake.Close()
	s := server.NewMCPServer("chat-test", "1")
	registerChatTools(s, func(context.Context) (*xrpc.Client, error) {
		return &xrpc.Client{Host: fake.URL, Client: fake.Client(), Auth: &xrpc.AuthInfo{Did: testDid}, Headers: map[string]string{"Atproto-Proxy": "did:web:api.bsky.chat#bsky_chat"}}, nil
	})
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Start(context.Background())
	var init mcp.InitializeRequest
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	if _, err := c.Initialize(context.Background(), init); err != nil {
		t.Fatal(err)
	}
	list := callOK(t, c, "bluesky_dm_list", map[string]any{})
	if !list["untrustedSocialContent"].(bool) {
		t.Fatal("missing untrusted content marker")
	}
	callOK(t, c, "bluesky_dm_messages", map[string]any{"convoId": "one"})
	preview := callOK(t, c, "bluesky_dm_send", map[string]any{"convoId": "one", "expectedDid": testDid, "text": "approved", "dryRun": true})
	if get(preview, "data", "dryRun") != true || writes != 0 {
		t.Fatalf("preview wrote: %d", writes)
	}
	for _, args := range []map[string]any{
		{"convoId": "one", "expectedDid": "did:plc:wrong", "text": "approved"},
		{"convoId": "one", "expectedDid": testDid, "text": ""},
		{"convoId": "one", "expectedDid": testDid, "text": strings.Repeat("a", 1001)},
		{"convoId": "one", "expectedDid": testDid, "text": "approved", "url": "http://localhost"},
	} {
		_, isErr := callTool(t, c, "bluesky_dm_send", args)
		if !isErr {
			t.Fatal("invalid args accepted")
		}
	}
	if writes != 0 {
		t.Fatal("invalid request wrote")
	}
	callOK(t, c, "bluesky_dm_send", map[string]any{"convoId": "one", "expectedDid": testDid, "text": "approved"})
	if writes != 1 {
		t.Fatalf("send writes %d", writes)
	}
	callOK(t, c, "bluesky_dm_mark_read", map[string]any{"convoId": "one", "expectedDid": testDid, "messageId": "new", "dryRun": true})
	if writes != 1 {
		t.Fatal("read preview wrote")
	}
	callOK(t, c, "bluesky_dm_mark_read", map[string]any{"convoId": "one", "expectedDid": testDid, "messageId": "new"})
	if writes != 2 || reads < 4 {
		t.Fatalf("unexpected writes/reads %d %d", writes, reads)
	}
	_, isErr := callTool(t, c, "bluesky_dm_list", map[string]any{"limit": 101})
	if !isErr {
		t.Fatal("unbounded listing")
	}
	_, isErr = callTool(t, c, "bluesky_dm_messages", map[string]any{"convoId": "one", "limit": 0})
	if !isErr {
		t.Fatal("zero limit accepted")
	}
}
