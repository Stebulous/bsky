package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

type chatRoundTrip func(*http.Request) (*http.Response, error)

func (f chatRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestChatDIDRedirectCannotSelectPDS(t *testing.T) {
	for _, location := range []string{"https://attacker.example/doc", "https://plc.directory/other", "//attacker.example/doc", "/other"} {
		t.Run(location, func(t *testing.T) {
			calls := 0
			transport := chatRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("Authorization") != "" {
					t.Fatal("session sent to DID resolver")
				}
				if calls == 1 {
					return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{location}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"` + testDid + `","service":[{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"https://attacker.example"}]}`)), Request: r}, nil
			})
			original := &xrpc.Client{Host: "https://original.example", Auth: &xrpc.AuthInfo{Did: testDid, AccessJwt: "synthetic-token"}}
			got, err := resolveChatPDS(context.Background(), original, newChatHTTPClient(transport))
			if err == nil || got != nil || calls != 1 || original.Host != "https://original.example" {
				t.Fatalf("redirect selected PDS: %v, calls=%d", err, calls)
			}
		})
	}
}

func TestChatDirectDIDDocument(t *testing.T) {
	transport := chatRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://plc.directory/"+url.PathEscape(testDid) || r.Header.Get("Authorization") != "" {
			t.Fatal("incorrect resolver request")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"` + testDid + `","service":[{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"https://pds.example"}]}`)), Request: r}, nil
	})
	got, err := resolveChatPDS(context.Background(), &xrpc.Client{Auth: &xrpc.AuthInfo{Did: testDid, AccessJwt: "synthetic-token"}}, newChatHTTPClient(transport))
	if err != nil || got.Host != "https://pds.example" || got.Auth.AccessJwt != "synthetic-token" || got.Headers["Atproto-Proxy"] == "" {
		t.Fatalf("direct document failed: %v", err)
	}
	if got.Client.CheckRedirect(&http.Request{}, nil) != http.ErrUseLastResponse {
		t.Fatal("API redirects permitted")
	}
}

func TestChatDIDWebMapping(t *testing.T) {
	for did, want := range map[string]string{
		"did:web:example.com":                  "https://example.com/.well-known/did.json",
		"did:web:example.com:user:alice":       "https://example.com/user/alice/did.json",
		"did:web:example.com%3A443:user:alice": "https://example.com:443/user/alice/did.json",
	} {
		got, err := chatDIDEndpoint(did)
		if err != nil || got != want {
			t.Errorf("%s: got %s, %v", did, got, err)
		}
	}
	for _, did := range []string{"did:web:example.com%3A3000", "did:web:example.com@evil.example", "did:web:example.com?x", "did:web:example.com#x", "did:web:example.com/path", "did:web:example.com:%2e%2e", "did:web:example.com:a%2fb", "did:web:example.com:", "did:web:"} {
		if got, err := chatDIDEndpoint(did); err == nil {
			t.Errorf("accepted unsafe/unsupported DID %s: %s", did, got)
		}
	}
}

func TestDMAuthSetupHonorsDeadline(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(fmt.Sprint(refresh), func(t *testing.T) {
			dir := t.TempDir()
			if refresh {
				if err := os.WriteFile(filepath.Join(dir, "fixture.auth"), []byte(`{"did":"`+testDid+`","handle":"fixture","accessJwt":"synthetic-access","refreshJwt":"synthetic-refresh"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			transport := chatRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			_, err := makeXRPCCFromConfigContext(ctx, &config{dir: dir, Handle: "fixture", Host: "https://auth.example", Password: "synthetic-password"}, newChatHTTPClient(transport))
			if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
				t.Fatalf("setup escaped deadline/fell back after timeout: %v calls=%d", err, calls)
			}
		})
	}
}

func TestChatDIDRejectsUntrustedDocuments(t *testing.T) {
	for _, body := range []string{
		`{"id":"did:plc:wrong","service":[]}`,
		`{"id":"` + testDid + `","service":[{"id":"#other","type":"AtprotoPersonalDataServer","serviceEndpoint":"https://pds.example"}]}`,
		`{"id":"` + testDid + `","service":[{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"https://pds.example/path"}]}`,
		`{"id":"` + testDid + `","service":[{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"http://pds.example"}]}`,
		`{"id":"` + testDid + `"}` + strings.Repeat(" ", 1<<20),
	} {
		transport := chatRoundTrip(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})
		if got, err := resolveChatPDS(context.Background(), &xrpc.Client{Host: "https://original.example", Auth: &xrpc.AuthInfo{Did: testDid}}, newChatHTTPClient(transport)); err == nil || got != nil {
			t.Fatal("accepted invalid DID document")
		}
	}
}

func TestDMAuthConfiguredEntryway(t *testing.T) {
	calls := 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/xrpc/com.atproto.server.createSession" {
			t.Errorf("unexpected auth path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"did":"`+testDid+`","handle":"fixture.example","accessJwt":"synthetic-access","refreshJwt":"synthetic-refresh"}`)
	}))
	defer fake.Close()
	c, err := makeXRPCCFromConfigContext(context.Background(), &config{dir: t.TempDir(), Handle: "fixture", Host: fake.URL, Password: "synthetic-password"}, newChatAuthHTTPClient())
	if err != nil || c.Auth.Did != testDid || calls != 1 {
		t.Fatalf("configured authentication entryway failed: %v calls=%d", err, calls)
	}
}
