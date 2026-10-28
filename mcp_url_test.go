package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// newImageHost serves test images over TLS on 127.0.0.1. Tests reach it
// through a fetcher that uses the test server's transport, which bypasses
// the public-address check; TestImageFetcherBlocksNonPublicAddresses covers
// that check with the real fetcher.
func newImageHost(t *testing.T) (*httptest.Server, *imageFetcher) {
	t.Helper()
	png := testPNG(t, 30, 20)
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/widget.png":
			w.Header().Set("Content-Type", "text/plain") // the type is sniffed, not trusted
			w.Write(png)
		case "/big.png":
			// Chunked response without Content-Length.
			w.Write(png)
			w.(http.Flusher).Flush()
			w.Write(make([]byte, maxImageBytes))
		case "/declared-big.png":
			w.Header().Set("Content-Length", "2000000")
			w.WriteHeader(http.StatusOK)
		case "/to-http":
			http.Redirect(w, r, "http://"+r.Host+"/widget.png", http.StatusFound)
		case "/to-widget":
			http.Redirect(w, r, "/widget.png", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &imageFetcher{client: newImageHTTPClient(srv.Client().Transport)}
}

func TestPostImageURL(t *testing.T) {
	pds, srv := newFakePDS(t)
	host, urls := newImageHost(t)
	c := newTestClientWithURLs(t, srv, nil, urls)
	post := func(u string) (string, bool) {
		return callTool(t, c, "bluesky_post", map[string]any{"text": "a", "images": []any{map[string]any{"url": u, "alt": "Widget"}}})
	}

	for _, u := range []string{host.URL + "/widget.png", host.URL + "/to-widget"} {
		text, isErr := post(u)
		mustOK(t, text, isErr)
	}
	if len(pds.uploads) != 2 || pds.uploads[0].ContentType != "image/png" {
		t.Fatalf("uploads: %+v", pds.uploads)
	}
	if img := get(decodeRecord(t, pds.writes[0]), "embed", "images", 0); get(img, "aspectRatio", "width") != float64(30) || get(img, "alt") != "Widget" {
		t.Errorf("image: %v", img)
	}

	for _, tc := range []struct{ path, want string }{
		{"/big.png", "larger than 1000000 bytes"},
		{"/declared-big.png", "the limit is 1000000 bytes"},
		{"/missing.png", "HTTP 404"},
		{"/to-http", "must be an absolute https:// URL"},
		{"/loop", "too many redirects"},
	} {
		text, isErr := post(host.URL + tc.path)
		mustFail(t, text, isErr, tc.want)
	}
	for _, tc := range []struct{ url, want string }{
		{"http://example.com/a.png", "must be an absolute https:// URL"},
		{"https://user:pass@example.com/a.png", "without credentials"},
		{"/relative.png", "must be an absolute https:// URL"},
	} {
		text, isErr := post(tc.url)
		mustFail(t, text, isErr, tc.want)
		if strings.Contains(text, "pass@") {
			t.Errorf("error message leaks URL credentials: %s", text)
		}
	}
	if len(pds.uploads) != 2 || len(pds.writes) != 2 {
		t.Errorf("rejected URLs reached the PDS: %d uploads, %d writes", len(pds.uploads), len(pds.writes))
	}
}

func TestImageFetcherBlocksNonPublicAddresses(t *testing.T) {
	host, _ := newImageHost(t)
	f := newImageFetcher()
	for _, u := range []string{
		host.URL + "/widget.png", // 127.0.0.1 on a non-443 port
		"https://127.0.0.1/widget.png",
		"https://[::1]/widget.png",
		"https://10.0.0.1/widget.png",
		"https://169.254.169.254/latest/meta-data",
		"https://localhost/widget.png",
	} {
		_, err := f.fetch(context.Background(), u)
		if err == nil || !strings.Contains(err.Error(), "non-public address") {
			t.Errorf("fetch(%q) = %v, want a non-public address error", u, err)
		}
	}
}

func TestIsPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"185.199.108.133":     true, // raw.githubusercontent.com
		"2606:50c0:8000::154": true,
		"127.0.0.1":           false,
		"10.1.2.3":            false,
		"172.16.0.1":          false,
		"192.168.1.1":         false,
		"169.254.169.254":     false,
		"100.64.0.1":          false,
		"0.0.0.0":             false,
		"224.0.0.1":           false,
		"255.255.255.255":     false,
		"::1":                 false,
		"fe80::1":             false,
		"fc00::1":             false,
		"::ffff:127.0.0.1":    false,
		"::ffff:8.8.8.8":      true,
		"::ffff:100.64.0.1":   false,
		"64:ff9b::a00:1":      false,
		"2002:a00:1::":        false,
	} {
		if got := isPublicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("isPublicAddr(%s) = %v, want %v", addr, got, want)
		}
	}
}
