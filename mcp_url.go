package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// imageFetcher downloads images from public HTTPS URLs for bluesky_post.
// A nil *imageFetcher means that image URLs are disabled.
type imageFetcher struct {
	client *http.Client
}

// nonPublicPrefixes are address ranges that are not reachable on the public
// internet, or that can map to such addresses, in addition to the ranges
// that netip.Addr methods already report.
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
}

func isPublicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range nonPublicPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// checkPublicDial rejects connections to non-public addresses and to ports
// other than 443. It runs on the resolved address of every connection,
// including redirects, so DNS rebinding cannot reach internal hosts.
func checkPublicDial(network, address string, _ syscall.RawConn) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	if port != "443" || !isPublicAddr(ip) {
		return fmt.Errorf("image URL resolves to a non-public address")
	}
	return nil
}

func newImageFetcher() *imageFetcher {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: checkPublicDial}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            dialer.DialContext,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  10 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
	}
	return &imageFetcher{client: newImageHTTPClient(transport)}
}

func newImageHTTPClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			return checkImageURL(req.URL)
		},
	}
}

func checkImageURL(u *url.URL) error {
	if u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fmt.Errorf("image URL %q must be an absolute https:// URL without credentials", u.Redacted())
	}
	return nil
}

// fetch downloads at most maxImageBytes from rawURL. The content type is
// detected from the data later, not taken from the response headers.
func (f *imageFetcher) fetch(ctx context.Context, rawURL string) ([]byte, error) {
	if f == nil {
		return nil, errors.New("image URLs are disabled on this server; send base64 instead, or start the server with --allow-image-urls")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid image URL: %w", err)
	}
	if err := checkImageURL(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot download image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cannot download image: HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxImageBytes {
		return nil, fmt.Errorf("image is %d bytes; the limit is %d bytes", resp.ContentLength, maxImageBytes)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cannot download image: %w", err)
	}
	if len(b) > maxImageBytes {
		return nil, fmt.Errorf("image is larger than %d bytes", maxImageBytes)
	}
	return b, nil
}
