package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

const testDid = "did:plc:testaccount"

// fakePDS implements the XRPC methods that the MCP tools call. It records
// every write so that tests can check what would reach a real PDS.
type fakePDS struct {
	t  *testing.T
	mu sync.Mutex

	posts       map[string]map[string]any // rkey -> post record
	profile     map[string]json.RawMessage
	profileCid  string
	uploads     []fakeUpload
	writes      []fakeWrite
	beforeWrite func() // runs before a profile write is applied
}

type fakeUpload struct {
	ContentType string
	Size        int
}

type fakeWrite struct {
	Method string
	Body   map[string]json.RawMessage
}

func newFakePDS(t *testing.T) (*fakePDS, *httptest.Server) {
	p := &fakePDS{t: t, posts: map[string]map[string]any{}}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return p, srv
}

func testCid(data string) string {
	c, err := cid.Prefix{Version: 1, Codec: cid.Raw, MhType: 0x12, MhLength: -1}.Sum([]byte(data))
	if err != nil {
		panic(err)
	}
	return c.String()
}

func writeXRPCError(w http.ResponseWriter, name, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{"error": name, "message": msg})
}

func (p *fakePDS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	q := r.URL.Query()
	switch strings.TrimPrefix(r.URL.Path, "/xrpc/") {
	case "com.atproto.server.createSession":
		reply(map[string]string{"did": testDid, "handle": "test.example", "accessJwt": "access", "refreshJwt": "refresh"})
	case "com.atproto.repo.uploadBlob":
		b, _ := io.ReadAll(r.Body)
		p.uploads = append(p.uploads, fakeUpload{ContentType: r.Header.Get("Content-Type"), Size: len(b)})
		reply(map[string]any{"blob": map[string]any{
			"$type": "blob", "ref": map[string]string{"$link": testCid(string(b))},
			"mimeType": r.Header.Get("Content-Type"), "size": len(b),
		}})
	case "com.atproto.repo.getRecord":
		switch q.Get("collection") {
		case "app.bsky.feed.post":
			rec, ok := p.posts[q.Get("rkey")]
			if !ok {
				writeXRPCError(w, "RecordNotFound", "no post")
				return
			}
			uri := "at://" + q.Get("repo") + "/app.bsky.feed.post/" + q.Get("rkey")
			reply(map[string]any{"uri": uri, "cid": testCid(uri), "value": rec})
		case profileCollection:
			if p.profile == nil {
				writeXRPCError(w, "RecordNotFound", "no profile")
				return
			}
			reply(map[string]any{"uri": "at://" + testDid + "/" + profileCollection + "/self", "cid": p.profileCid, "value": p.profile})
		default:
			writeXRPCError(w, "InvalidRequest", "unexpected collection")
		}
	case "app.bsky.actor.getProfile":
		if q.Get("actor") != "friend.example" {
			writeXRPCError(w, "InvalidRequest", "Profile not found")
			return
		}
		reply(map[string]string{"did": "did:plc:friend", "handle": "friend.example"})
	case "com.atproto.repo.createRecord", "com.atproto.repo.putRecord":
		method := strings.TrimPrefix(r.URL.Path, "/xrpc/")
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeXRPCError(w, "InvalidRequest", err.Error())
			return
		}
		var collection string
		json.Unmarshal(body["collection"], &collection)
		if collection == profileCollection {
			if p.beforeWrite != nil {
				p.beforeWrite()
			}
			var swap *string
			json.Unmarshal(body["swapRecord"], &swap)
			switch {
			case method == "com.atproto.repo.createRecord" && p.profile != nil:
				writeXRPCError(w, "InvalidRequest", "Record already exists")
				return
			case method == "com.atproto.repo.putRecord" && (swap == nil || *swap != p.profileCid):
				writeXRPCError(w, "InvalidSwap", "Record was at "+p.profileCid)
				return
			}
			p.writes = append(p.writes, fakeWrite{Method: method, Body: body})
			json.Unmarshal(body["record"], &p.profile)
			p.profileCid = testCid(string(body["record"]))
			reply(map[string]string{"uri": "at://" + testDid + "/" + profileCollection + "/self", "cid": p.profileCid})
			return
		}
		p.writes = append(p.writes, fakeWrite{Method: method, Body: body})
		reply(map[string]string{"uri": "at://" + testDid + "/app.bsky.feed.post/newpost", "cid": testCid("newpost")})
	default:
		writeXRPCError(w, "MethodNotImplemented", r.URL.Path)
	}
}

func newTestClient(t *testing.T, srv *httptest.Server, media *mediaDir) *client.Client {
	t.Helper()
	cfg := &config{Host: srv.URL, Handle: "test.example", Password: "app-password", dir: t.TempDir()}
	c, err := client.NewInProcessClient(newMCPServer(cfg, media))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var init mcp.InitializeRequest
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	if _, err := c.Initialize(ctx, init); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// callTool returns the text result and whether the tool reported an error.
func callTool(t *testing.T, c *client.Client, name string, args map[string]any) (string, bool) {
	t.Helper()
	var req mcp.CallToolRequest
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := c.CallTool(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, content := range res.Content {
		if tc, ok := content.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String(), res.IsError
}

func callOK(t *testing.T, c *client.Client, name string, args map[string]any) map[string]any {
	t.Helper()
	text, isErr := callTool(t, c, name, args)
	return mustOK(t, text, isErr)
}

func mustOK(t *testing.T, text string, isErr bool) map[string]any {
	t.Helper()
	if isErr {
		t.Fatalf("tool error: %s", text)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("result is not JSON: %v: %s", err, text)
	}
	return m
}

func mustFail(t *testing.T, text string, isErr bool, want string) {
	t.Helper()
	if !isErr {
		t.Fatalf("want error containing %q, got success: %s", want, text)
	}
	if !strings.Contains(text, want) {
		t.Fatalf("want error containing %q, got %q", want, text)
	}
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testAPNG inserts an acTL chunk after IHDR, which marks an animated PNG.
func testAPNG(t *testing.T) []byte {
	b := testPNG(t, 2, 2)
	actl := []byte{0, 0, 0, 8, 'a', 'c', 'T', 'L', 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0}
	return append(append(append([]byte{}, b[:33]...), actl...), b[33:]...)
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func decodeRecord(t *testing.T, w fakeWrite) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal(w.Body["record"], &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// get follows a path of map keys and slice indexes through decoded JSON.
func get(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			s, _ := v.([]any)
			if k >= len(s) {
				return nil
			}
			v = s[k]
		}
	}
	return v
}

func TestMCPToolList(t *testing.T) {
	_, srv := newFakePDS(t)
	c := newTestClient(t, srv, nil)
	res, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]mcp.Tool{}
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}
	for _, name := range []string{"bluesky_timeline", "bluesky_post", "bluesky_search", "bluesky_show_profile", "bluesky_search_actors",
		"bluesky_thread", "bluesky_notification", "bluesky_like", "bluesky_repost", "bluesky_follow", "bluesky_update_profile"} {
		if _, ok := tools[name]; !ok {
			t.Errorf("tool %s is missing", name)
		}
	}

	b, _ := json.Marshal(tools["bluesky_post"].InputSchema)
	var post map[string]any
	json.Unmarshal(b, &post)
	if get(post, "additionalProperties") != false {
		t.Errorf("bluesky_post must reject additional properties: %s", b)
	}
	if get(post, "properties", "images", "maxItems") != float64(4) {
		t.Errorf("images.maxItems must be 4: %s", b)
	}
	if req := get(post, "properties", "images", "items", "required"); len(req.([]any)) != 1 || req.([]any)[0] != "alt" {
		t.Errorf("images items must require alt: %s", b)
	}

	b, _ = json.Marshal(tools["bluesky_update_profile"].InputSchema)
	var profile map[string]any
	json.Unmarshal(b, &profile)
	if req := get(profile, "required"); len(req.([]any)) != 1 || req.([]any)[0] != "expectedDid" {
		t.Errorf("bluesky_update_profile must require expectedDid: %s", b)
	}
	if get(profile, "properties", "displayName", "maxLength") != float64(640) || get(profile, "properties", "description", "maxLength") != float64(2560) {
		t.Errorf("profile text limits changed: %s", b)
	}
	for _, field := range []string{"avatarBase64", "bannerBase64", "website", "dryRun"} {
		if get(profile, "properties", field) == nil {
			t.Errorf("profile schema has no %s", field)
		}
	}
	if a := tools["bluesky_update_profile"].Annotations; a.DestructiveHint == nil || *a.DestructiveHint {
		t.Errorf("profile tool must not be marked destructive")
	}
}

func TestPostReplyWithImages(t *testing.T) {
	pds, srv := newFakePDS(t)
	rootURI := "at://did:plc:other/app.bsky.feed.post/root"
	pds.posts["parent"] = map[string]any{
		"$type": "app.bsky.feed.post", "text": "parent", "createdAt": "2026-10-01T00:00:00Z",
		"reply": map[string]any{
			"root":   map[string]string{"uri": rootURI, "cid": testCid(rootURI)},
			"parent": map[string]string{"uri": rootURI, "cid": testCid(rootURI)},
		},
	}
	c := newTestClient(t, srv, nil)
	pngData, jpegData := testPNG(t, 1200, 600), testJPEG(t, 300, 400)
	out := callOK(t, c, "bluesky_post", map[string]any{
		"text":  "Screenshots https://example.com/repo cc @friend.example #golang",
		"reply": "https://bsky.app/profile/did:plc:other/post/parent",
		"images": []any{
			map[string]any{"base64": b64(pngData), "alt": "Dashboard screenshot"},
			map[string]any{"base64": "data:image/jpeg;base64," + b64(jpegData), "alt": " Settings page "},
		},
	})
	if out["uri"] != "at://"+testDid+"/app.bsky.feed.post/newpost" || out["cid"] == "" ||
		out["url"] != "https://bsky.app/profile/"+testDid+"/post/newpost" || out["images"] != float64(2) {
		t.Errorf("unexpected result: %v", out)
	}

	if len(pds.uploads) != 2 || pds.uploads[0] != (fakeUpload{"image/png", len(pngData)}) || pds.uploads[1] != (fakeUpload{"image/jpeg", len(jpegData)}) {
		t.Errorf("uploads must use the detected content type: %+v", pds.uploads)
	}
	if len(pds.writes) != 1 {
		t.Fatalf("want 1 write, got %d", len(pds.writes))
	}
	rec := decodeRecord(t, pds.writes[0])
	if get(rec, "embed", "$type") != "app.bsky.embed.images" {
		t.Fatalf("embed type: %v", get(rec, "embed"))
	}
	for i, want := range []struct {
		alt  string
		mime string
		w, h float64
	}{{"Dashboard screenshot", "image/png", 1200, 600}, {"Settings page", "image/jpeg", 300, 400}} {
		img := get(rec, "embed", "images", i)
		if get(img, "alt") != want.alt || get(img, "image", "$type") != "blob" || get(img, "image", "mimeType") != want.mime ||
			get(img, "image", "ref", "$link") == nil || get(img, "aspectRatio", "width") != want.w || get(img, "aspectRatio", "height") != want.h {
			t.Errorf("image %d: %v", i, img)
		}
	}
	parentURI := "at://did:plc:other/app.bsky.feed.post/parent"
	if get(rec, "reply", "parent", "uri") != parentURI || get(rec, "reply", "parent", "cid") != testCid(parentURI) ||
		get(rec, "reply", "root", "uri") != rootURI || get(rec, "reply", "root", "cid") != testCid(rootURI) {
		t.Errorf("reply refs: %v", get(rec, "reply"))
	}
	features := map[string]bool{}
	for _, f := range get(rec, "facets").([]any) {
		features[get(f, "features", 0, "$type").(string)] = true
	}
	for _, want := range []string{"app.bsky.richtext.facet#link", "app.bsky.richtext.facet#mention", "app.bsky.richtext.facet#tag"} {
		if !features[want] {
			t.Errorf("facet %s is missing: %v", want, get(rec, "facets"))
		}
	}
}

func TestPostQuoteWithImages(t *testing.T) {
	pds, srv := newFakePDS(t)
	pds.posts["quoted"] = map[string]any{"$type": "app.bsky.feed.post", "text": "quoted", "createdAt": "2026-10-01T00:00:00Z"}
	c := newTestClient(t, srv, nil)
	callOK(t, c, "bluesky_post", map[string]any{
		"text":   "look",
		"quote":  "at://did:plc:other/app.bsky.feed.post/quoted",
		"images": []any{map[string]any{"base64": b64(testPNG(t, 10, 10)), "alt": "Chart"}},
	})
	rec := decodeRecord(t, pds.writes[0])
	embed := get(rec, "embed")
	if get(embed, "$type") != "app.bsky.embed.recordWithMedia" ||
		get(embed, "record", "$type") != "app.bsky.embed.record" ||
		get(embed, "record", "record", "uri") != "at://did:plc:other/app.bsky.feed.post/quoted" ||
		get(embed, "media", "$type") != "app.bsky.embed.images" ||
		get(embed, "media", "images", 0, "alt") != "Chart" {
		t.Errorf("recordWithMedia embed: %v", embed)
	}

	// A quote without images keeps the plain record embed.
	callOK(t, c, "bluesky_post", map[string]any{"text": "plain", "quote": "at://did:plc:other/app.bsky.feed.post/quoted"})
	if embed := get(decodeRecord(t, pds.writes[1]), "embed"); get(embed, "$type") != "app.bsky.embed.record" {
		t.Errorf("quote embed: %v", embed)
	}
}

func TestPostRejectsInvalidInputBeforeUpload(t *testing.T) {
	pds, srv := newFakePDS(t)
	c := newTestClient(t, srv, nil)
	good := b64(testPNG(t, 4, 4))
	gifBuf := new(bytes.Buffer)
	gif.Encode(gifBuf, image.NewPaletted(image.Rect(0, 0, 2, 2), []color.Color{color.Black}), nil)
	oversize := append(testPNG(t, 2, 2), make([]byte, maxImageBytes)...)
	five := []any{}
	for range 5 {
		five = append(five, map[string]any{"base64": good, "alt": "x"})
	}
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"missing alt", map[string]any{"text": "a", "images": []any{map[string]any{"base64": good}}}, "images[0]: alt text is required"},
		{"blank alt", map[string]any{"text": "a", "images": []any{map[string]any{"base64": good, "alt": "  "}}}, "alt text is required"},
		{"too many", map[string]any{"text": "a", "images": five}, "at most 4"},
		{"oversize", map[string]any{"text": "a", "images": []any{map[string]any{"base64": b64(oversize), "alt": "x"}}}, "the limit is 1000000 bytes"},
		{"gif", map[string]any{"text": "a", "images": []any{map[string]any{"base64": b64(gifBuf.Bytes()), "alt": "x"}}}, "unsupported image type image/gif"},
		{"apng", map[string]any{"text": "a", "images": []any{map[string]any{"base64": b64(testAPNG(t)), "alt": "x"}}}, "animated images are not supported"},
		{"not base64", map[string]any{"text": "a", "images": []any{map[string]any{"base64": "%%%", "alt": "x"}}}, "not valid base64"},
		{"both sources", map[string]any{"text": "a", "images": []any{map[string]any{"base64": good, "path": "a.png", "alt": "x"}}}, "not both"},
		{"no source", map[string]any{"text": "a", "images": []any{map[string]any{"alt": "x"}}}, "base64 or path is required"},
		{"paths disabled", map[string]any{"text": "a", "images": []any{map[string]any{"path": "a.png", "alt": "x"}}}, "image paths are disabled"},
		{"second image bad", map[string]any{"text": "a", "images": []any{map[string]any{"base64": good, "alt": "x"}, map[string]any{"base64": good}}}, "images[1]"},
		{"unknown argument", map[string]any{"text": "a", "image": good}, "unknown field \"image\""},
		{"empty post", map[string]any{"text": " "}, "text is required"},
		{"long text", map[string]any{"text": strings.Repeat("a", 3001)}, "the limit is 3000 bytes"},
		{"bad reply uri", map[string]any{"text": "a", "reply": "https://example.com/x"}, "invalid post uri"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, isErr := callTool(t, c, "bluesky_post", tc.args)
			mustFail(t, text, isErr, tc.want)
		})
	}
	if len(pds.uploads) != 0 || len(pds.writes) != 0 {
		t.Errorf("invalid input reached the PDS: %d uploads, %d writes", len(pds.uploads), len(pds.writes))
	}
}

func TestPostImageOnlyAndDryRun(t *testing.T) {
	pds, srv := newFakePDS(t)
	c := newTestClient(t, srv, nil)
	images := []any{map[string]any{"base64": b64(testPNG(t, 16, 9)), "alt": "Logo"}}
	out := callOK(t, c, "bluesky_post", map[string]any{"text": "", "images": images, "dryRun": true})
	if out["dryRun"] != true || get(out, "record", "embed", "images", 0, "aspectRatio", "width") != float64(16) || get(out, "images", 0, "mimeType") != "image/png" {
		t.Errorf("dry run result: %v", out)
	}
	if len(pds.uploads) != 0 || len(pds.writes) != 0 {
		t.Fatalf("dry run reached the PDS: %d uploads, %d writes", len(pds.uploads), len(pds.writes))
	}
	callOK(t, c, "bluesky_post", map[string]any{"text": "", "images": images})
	if len(pds.uploads) != 1 || len(pds.writes) != 1 {
		t.Errorf("image-only post: %d uploads, %d writes", len(pds.uploads), len(pds.writes))
	}
}

func TestPostImagePaths(t *testing.T) {
	pds, srv := newFakePDS(t)
	base := t.TempDir()
	dir := filepath.Join(base, "media")
	os.Mkdir(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "shot.png"), testPNG(t, 8, 8), 0o600)
	os.WriteFile(filepath.Join(base, "secret.png"), testPNG(t, 8, 8), 0o600)
	os.Symlink(filepath.Join(base, "secret.png"), filepath.Join(dir, "link.png"))
	os.WriteFile(filepath.Join(dir, "big.png"), append(testPNG(t, 2, 2), make([]byte, maxImageBytes)...), 0o600)
	media, err := openMediaDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, srv, media)
	post := func(path string) (string, bool) {
		return callTool(t, c, "bluesky_post", map[string]any{"text": "a", "images": []any{map[string]any{"path": path, "alt": "x"}}})
	}
	for _, p := range []string{"shot.png", filepath.Join(dir, "shot.png")} {
		text, isErr := post(p)
		mustOK(t, text, isErr)
	}
	text, isErr := post("../secret.png")
	mustFail(t, text, isErr, "cannot open image")
	text, isErr = post(filepath.Join(base, "secret.png"))
	mustFail(t, text, isErr, "outside the media directory")
	text, isErr = post("link.png")
	mustFail(t, text, isErr, "cannot open image")
	text, isErr = post("big.png")
	mustFail(t, text, isErr, "the limit is 1000000 bytes")
	if len(pds.uploads) != 2 {
		t.Errorf("want 2 uploads, got %d", len(pds.uploads))
	}
}

func seedProfile(pds *fakePDS) {
	pds.profile = map[string]json.RawMessage{
		"$type":       json.RawMessage(`"app.bsky.actor.profile"`),
		"displayName": json.RawMessage(`"Old Name"`),
		"description": json.RawMessage(`"Old description"`),
		"avatar":      json.RawMessage(`{"$type":"blob","ref":{"$link":"` + testCid("avatar") + `"},"mimeType":"image/jpeg","size":1234}`),
		"pinnedPost":  json.RawMessage(`{"uri":"at://` + testDid + `/app.bsky.feed.post/pinned","cid":"` + testCid("pinned") + `"}`),
		"pronouns":    json.RawMessage(`"they/them"`),
		"labels":      json.RawMessage(`{"$type":"com.atproto.label.defs#selfLabels","values":[{"val":"!no-unauthenticated"}]}`),
		"createdAt":   json.RawMessage(`"2024-01-01T00:00:00.000Z"`),
	}
	pds.profileCid = testCid("profile-v1")
}

func TestUpdateProfilePreservesUnrelatedFields(t *testing.T) {
	pds, srv := newFakePDS(t)
	seedProfile(pds)
	prior, _ := json.Marshal(pds.profile)
	c := newTestClient(t, srv, nil)
	banner := testJPEG(t, 1500, 500)
	out := callOK(t, c, "bluesky_update_profile", map[string]any{
		"expectedDid": testDid, "displayName": "New Name", "bannerBase64": b64(banner), "website": "https://example.com/about",
	})
	if len(pds.writes) != 1 || pds.writes[0].Method != "com.atproto.repo.putRecord" {
		t.Fatalf("writes: %+v", pds.writes)
	}
	var swap string
	json.Unmarshal(pds.writes[0].Body["swapRecord"], &swap)
	if swap != testCid("profile-v1") {
		t.Errorf("swapRecord = %q, want the prior cid", swap)
	}
	rec := decodeRecord(t, pds.writes[0])
	var priorRec map[string]any
	json.Unmarshal(prior, &priorRec)
	for _, field := range []string{"$type", "description", "avatar", "pinnedPost", "pronouns", "labels", "createdAt"} {
		a, _ := json.Marshal(rec[field])
		b, _ := json.Marshal(priorRec[field])
		if !bytes.Equal(a, b) {
			t.Errorf("%s changed: %s -> %s", field, b, a)
		}
	}
	if rec["displayName"] != "New Name" || rec["website"] != "https://example.com/about" ||
		get(rec, "banner", "$type") != "blob" || get(rec, "banner", "mimeType") != "image/jpeg" || get(rec, "banner", "size") != float64(len(banner)) {
		t.Errorf("updated fields: %v", rec)
	}
	if len(pds.uploads) != 1 || pds.uploads[0].ContentType != "image/jpeg" {
		t.Errorf("uploads: %+v", pds.uploads)
	}
	got, _ := json.Marshal(out["priorRecord"])
	var gotRec map[string]any
	json.Unmarshal(got, &gotRec)
	a, _ := json.Marshal(gotRec)
	b, _ := json.Marshal(priorRec)
	if !bytes.Equal(a, b) || out["priorCid"] != testCid("profile-v1") || out["cid"] == "" {
		t.Errorf("rollback data: priorCid=%v priorRecord=%s", out["priorCid"], got)
	}
	if changed, _ := json.Marshal(out["changed"]); string(changed) != `["banner","displayName","website"]` {
		t.Errorf("changed = %s", changed)
	}
}

func TestUpdateProfileConcurrentEditFails(t *testing.T) {
	pds, srv := newFakePDS(t)
	seedProfile(pds)
	pds.beforeWrite = func() {
		// Another client saves the profile between the read and the write.
		pds.profile["displayName"] = json.RawMessage(`"Edited elsewhere"`)
		pds.profileCid = testCid("profile-v2")
	}
	c := newTestClient(t, srv, nil)
	text, isErr := callTool(t, c, "bluesky_update_profile", map[string]any{"expectedDid": testDid, "description": "mine"})
	mustFail(t, text, isErr, "nothing was overwritten")
	if len(pds.writes) != 0 || string(pds.profile["displayName"]) != `"Edited elsewhere"` {
		t.Errorf("concurrent edit was overwritten")
	}
}

func TestUpdateProfileConcurrentCreateFails(t *testing.T) {
	pds, srv := newFakePDS(t)
	pds.beforeWrite = func() {
		// Another client creates the profile between the read and the write.
		seedProfile(pds)
	}
	c := newTestClient(t, srv, nil)
	text, isErr := callTool(t, c, "bluesky_update_profile", map[string]any{"expectedDid": testDid, "displayName": "mine"})
	mustFail(t, text, isErr, "nothing was overwritten")
	if len(pds.writes) != 0 || string(pds.profile["displayName"]) != `"Old Name"` {
		t.Errorf("concurrently created profile was overwritten")
	}
}

func TestUpdateProfileCreatesMissingRecord(t *testing.T) {
	pds, srv := newFakePDS(t)
	c := newTestClient(t, srv, nil)
	out := callOK(t, c, "bluesky_update_profile", map[string]any{"expectedDid": testDid, "avatarBase64": b64(testPNG(t, 400, 400))})
	if len(pds.writes) != 1 || pds.writes[0].Method != "com.atproto.repo.createRecord" || string(pds.writes[0].Body["rkey"]) != `"self"` {
		t.Fatalf("writes: %+v", pds.writes)
	}
	rec := decodeRecord(t, pds.writes[0])
	if rec["$type"] != profileCollection || get(rec, "avatar", "mimeType") != "image/png" || out["priorRecord"] != nil {
		t.Errorf("created record: %v, result %v", rec, out)
	}
}

func TestUpdateProfileClearsAndDryRun(t *testing.T) {
	pds, srv := newFakePDS(t)
	seedProfile(pds)
	c := newTestClient(t, srv, nil)
	out := callOK(t, c, "bluesky_update_profile", map[string]any{
		"expectedDid": testDid, "description": "", "avatarBase64": b64(testPNG(t, 4, 4)), "dryRun": true,
	})
	if out["dryRun"] != true || get(out, "proposedRecord", "description") != nil || get(out, "proposedRecord", "avatar", "pendingUpload") != true ||
		get(out, "proposedRecord", "pronouns") != "they/them" {
		t.Errorf("dry run: %v", out)
	}
	if len(pds.uploads) != 0 || len(pds.writes) != 0 {
		t.Fatalf("dry run reached the PDS")
	}
	callOK(t, c, "bluesky_update_profile", map[string]any{"expectedDid": testDid, "description": ""})
	if _, ok := decodeRecord(t, pds.writes[0])["description"]; ok {
		t.Errorf("empty description must clear the field")
	}
}

func TestUpdateProfileRejectsInvalidInput(t *testing.T) {
	pds, srv := newFakePDS(t)
	seedProfile(pds)
	c := newTestClient(t, srv, nil)
	webp := []byte("RIFF\x1a\x00\x00\x00WEBPVP8 ")
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"wrong account", map[string]any{"expectedDid": "did:plc:someoneelse", "displayName": "x"}, "nothing was changed"},
		{"not a did", map[string]any{"expectedDid": "test.example", "displayName": "x"}, "must be a DID"},
		{"nothing to update", map[string]any{"expectedDid": testDid}, "nothing to update"},
		{"http website", map[string]any{"expectedDid": testDid, "website": "http://example.com"}, "https://"},
		{"relative website", map[string]any{"expectedDid": testDid, "website": "example.com"}, "https://"},
		{"long name", map[string]any{"expectedDid": testDid, "displayName": strings.Repeat("é", 321)}, "the limit is 640"},
		{"webp avatar", map[string]any{"expectedDid": testDid, "avatarBase64": b64(webp)}, "use PNG, JPEG"},
		{"animated banner", map[string]any{"expectedDid": testDid, "bannerBase64": b64(testAPNG(t))}, "animated"},
		{"oversize avatar", map[string]any{"expectedDid": testDid, "avatarBase64": b64(append(testPNG(t, 2, 2), make([]byte, maxImageBytes)...))}, "the limit is 1000000 bytes"},
		{"unknown argument", map[string]any{"expectedDid": testDid, "avatar": "x"}, "unknown field \"avatar\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, isErr := callTool(t, c, "bluesky_update_profile", tc.args)
			mustFail(t, text, isErr, tc.want)
		})
	}
	if len(pds.uploads) != 0 || len(pds.writes) != 0 {
		t.Errorf("invalid input reached the PDS: %d uploads, %d writes", len(pds.uploads), len(pds.writes))
	}
}

func TestParsePostRef(t *testing.T) {
	for _, tc := range []struct{ in, repo, rkey string }{
		{"at://did:plc:abc/app.bsky.feed.post/3k", "did:plc:abc", "3k"},
		{"https://bsky.app/profile/alice.example/post/3k", "alice.example", "3k"},
		{"at://did:plc:abc/app.bsky.feed.like/3k", "", ""},
		{"at://did:plc:abc/app.bsky.feed.post/3k/extra", "", ""},
		{"https://bsky.app/profile/alice.example", "", ""},
	} {
		repo, rkey, err := parsePostRef(tc.in)
		if repo != tc.repo || rkey != tc.rkey || (err == nil) != (tc.repo != "") {
			t.Errorf("parsePostRef(%q) = %q, %q, %v", tc.in, repo, rkey, err)
		}
	}
}

func TestDecodeBase64Image(t *testing.T) {
	data := testPNG(t, 3, 3)
	enc := base64.StdEncoding.EncodeToString(data)
	for _, in := range []string{enc, base64.RawStdEncoding.EncodeToString(data), "data:image/png;base64," + enc, enc[:10] + "\n" + enc[10:]} {
		b, err := decodeBase64Image(in)
		if err != nil || !bytes.Equal(b, data) {
			t.Errorf("decodeBase64Image(%.20q...) failed: %v", in, err)
		}
	}
	if _, err := decodeBase64Image("data:image/png," + enc); err == nil {
		t.Error("non-base64 data URL must fail")
	}
}

func TestIsAnimatedWebP(t *testing.T) {
	header := []byte("RIFF\x00\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00")
	animated := append(append([]byte{}, header...), 0x02, 0, 0, 0)
	still := append(append([]byte{}, header...), 0x00, 0, 0, 0)
	if !isAnimatedImage(animated, "image/webp") || isAnimatedImage(still, "image/webp") {
		t.Error("VP8X animation flag detection is wrong")
	}
}
