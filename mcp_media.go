package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/api/bsky"
	lexutil "github.com/bluesky-social/indigo/lex/util"
)

const (
	// maxPostImages is the maximum number of images in app.bsky.embed.images.
	maxPostImages = 4
	// maxImageBytes is the maxSize of app.bsky.embed.images#image and of the
	// avatar and banner blobs in app.bsky.actor.profile.
	maxImageBytes = 1000000
	// maxPostTextBytes is the maxLength of app.bsky.feed.post#text. The PDS
	// also enforces the 300 grapheme limit.
	maxPostTextBytes = 3000
)

var (
	postImageTypes    = []string{"image/png", "image/jpeg", "image/webp"}
	profileImageTypes = []string{"image/png", "image/jpeg"}
)

// mediaDir gives MCP tools read access to image files below one directory.
// A nil *mediaDir means that file paths are disabled.
type mediaDir struct {
	dir  string
	root *os.Root
}

func openMediaDir(dir string) (*mediaDir, error) {
	if dir == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve media directory: %w", err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("cannot open media directory: %w", err)
	}
	return &mediaDir{dir: abs, root: root}, nil
}

// readImage reads an image file below the media directory. os.Root rejects
// paths and symbolic links that escape the directory.
func (m *mediaDir) readImage(name string) ([]byte, error) {
	if m == nil {
		return nil, errors.New("image paths are disabled on this server; send base64 instead, or start the server with --media-dir")
	}
	rel := name
	if filepath.IsAbs(name) {
		r, err := filepath.Rel(m.dir, filepath.Clean(name))
		if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("image path %q is outside the media directory", name)
		}
		rel = r
	}
	f, err := m.root.Open(rel)
	if err != nil {
		return nil, fmt.Errorf("cannot open image %q: %w", name, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("cannot stat image %q: %w", name, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("image path %q is not a regular file", name)
	}
	if fi.Size() > maxImageBytes {
		return nil, fmt.Errorf("image %q is %d bytes; the limit is %d bytes", name, fi.Size(), maxImageBytes)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read image %q: %w", name, err)
	}
	if len(b) > maxImageBytes {
		return nil, fmt.Errorf("image %q is larger than %d bytes", name, maxImageBytes)
	}
	return b, nil
}

// decodeBase64Image decodes standard base64, with or without padding, and an
// optional "data:<type>;base64," prefix. It checks the size limit before it
// decodes the data.
func decodeBase64Image(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
	if strings.HasPrefix(s, "data:") {
		i := strings.Index(s, ",")
		if i < 0 || !strings.HasSuffix(s[:i], ";base64") {
			return nil, errors.New("data URL must use base64 encoding")
		}
		s = s[i+1:]
	}
	if s == "" {
		return nil, errors.New("image data is empty")
	}
	if len(s) > base64.StdEncoding.EncodedLen(maxImageBytes) {
		return nil, fmt.Errorf("image is about %d bytes; the limit is %d bytes", base64.StdEncoding.DecodedLen(len(s)), maxImageBytes)
	}
	enc := base64.StdEncoding
	if !strings.HasSuffix(s, "=") && len(s)%4 != 0 {
		enc = base64.RawStdEncoding
	}
	b, err := enc.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("image is not valid base64: %w", err)
	}
	if len(b) > maxImageBytes {
		return nil, fmt.Errorf("image is %d bytes; the limit is %d bytes", len(b), maxImageBytes)
	}
	return b, nil
}

type imageInfo struct {
	MimeType string `json:"mimeType"`
	Size     int    `json:"size"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

// inspectImage detects the type from the content, rejects types that are not
// in allowed and animated images, and reads the dimensions.
func inspectImage(b []byte, allowed []string) (imageInfo, error) {
	mime := http.DetectContentType(b)
	ok := false
	for _, t := range allowed {
		ok = ok || t == mime
	}
	if !ok {
		names := make([]string, len(allowed))
		for i, t := range allowed {
			names[i] = strings.ToUpper(strings.TrimPrefix(t, "image/"))
		}
		return imageInfo{}, fmt.Errorf("unsupported image type %s; use %s", mime, strings.Join(names, ", "))
	}
	if isAnimatedImage(b, mime) {
		return imageInfo{}, errors.New("animated images are not supported; use a static image")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return imageInfo{}, fmt.Errorf("cannot decode %s image: %w", mime, err)
	}
	if cfg.Width < 1 || cfg.Height < 1 {
		return imageInfo{}, errors.New("image has no pixels")
	}
	return imageInfo{MimeType: mime, Size: len(b), Width: cfg.Width, Height: cfg.Height}, nil
}

// isAnimatedImage reports APNG (an acTL chunk before IDAT) and animated WebP
// (the animation flag in the VP8X header).
func isAnimatedImage(b []byte, mime string) bool {
	switch mime {
	case "image/png":
		for p := 8; p+8 <= len(b); {
			n := int(binary.BigEndian.Uint32(b[p:]))
			switch string(b[p+4 : p+8]) {
			case "acTL":
				return true
			case "IDAT":
				return false
			}
			if n < 0 || p+12+n > len(b) {
				return false
			}
			p += 12 + n
		}
	case "image/webp":
		return len(b) > 20 && string(b[12:16]) == "VP8X" && b[20]&0x02 != 0
	}
	return false
}

// uploadImageBlob uploads b with its real content type, so that the PDS
// records the correct mimeType.
func uploadImageBlob(ctx context.Context, c lexutil.LexClient, b []byte, mime string) (*lexutil.LexBlob, error) {
	var out comatproto.RepoUploadBlob_Output
	if err := c.LexDo(ctx, lexutil.Procedure, mime, "com.atproto.repo.uploadBlob", nil, bytes.NewReader(b), &out); err != nil {
		return nil, fmt.Errorf("cannot upload image: %w", err)
	}
	if out.Blob == nil {
		return nil, errors.New("cannot upload image: PDS returned no blob")
	}
	if out.Blob.MimeType != mime || out.Blob.Size != int64(len(b)) {
		return nil, fmt.Errorf("cannot upload image: PDS stored %s (%d bytes), want %s (%d bytes)", out.Blob.MimeType, out.Blob.Size, mime, len(b))
	}
	return out.Blob, nil
}

// parsePostRef accepts an at:// post URI or a https://bsky.app post URL and
// returns the repo (DID or handle) and record key.
func parsePostRef(s string) (repo, rkey string, err error) {
	var parts []string
	switch {
	case strings.HasPrefix(s, "at://"):
		parts = strings.Split(strings.TrimPrefix(s, "at://"), "/")
		if len(parts) == 3 && parts[1] == "app.bsky.feed.post" {
			repo, rkey = parts[0], parts[2]
		}
	case strings.HasPrefix(s, "https://bsky.app/profile/"):
		parts = strings.Split(strings.TrimPrefix(s, "https://bsky.app/profile/"), "/")
		if len(parts) == 3 && parts[1] == "post" {
			repo, rkey = parts[0], parts[2]
		}
	}
	if repo == "" || rkey == "" {
		return "", "", fmt.Errorf("invalid post uri %q; use at://{did}/app.bsky.feed.post/{rkey} or https://bsky.app/profile/{handle}/post/{rkey}", s)
	}
	return repo, rkey, nil
}

// getPost reads a post and returns a strong reference to it.
func getPost(ctx context.Context, c lexutil.LexClient, uri string) (*comatproto.RepoStrongRef, *bsky.FeedPost, error) {
	repo, rkey, err := parsePostRef(uri)
	if err != nil {
		return nil, nil, err
	}
	resp, err := comatproto.RepoGetRecord(ctx, c, "", "app.bsky.feed.post", repo, rkey)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot get post %q: %w", uri, err)
	}
	post, ok := resp.Value.Val.(*bsky.FeedPost)
	if !ok || resp.Cid == nil {
		return nil, nil, fmt.Errorf("record %q is not a post", uri)
	}
	return &comatproto.RepoStrongRef{Uri: resp.Uri, Cid: *resp.Cid}, post, nil
}

// postWebURL converts at://{did}/app.bsky.feed.post/{rkey} to a bsky.app URL.
func postWebURL(uri string) string {
	repo, rkey, err := parsePostRef(uri)
	if err != nil {
		return ""
	}
	return "https://bsky.app/profile/" + repo + "/post/" + rkey
}

// postEmbed returns the embed for images, a quoted post, or both
// (app.bsky.embed.recordWithMedia).
func postEmbed(images []*bsky.EmbedImages_Image, quote *comatproto.RepoStrongRef) *bsky.FeedPost_Embed {
	var embedImages *bsky.EmbedImages
	if len(images) > 0 {
		embedImages = &bsky.EmbedImages{LexiconTypeID: "app.bsky.embed.images", Images: images}
	}
	var embedRecord *bsky.EmbedRecord
	if quote != nil {
		embedRecord = &bsky.EmbedRecord{LexiconTypeID: "app.bsky.embed.record", Record: quote}
	}
	switch {
	case embedImages != nil && embedRecord != nil:
		return &bsky.FeedPost_Embed{EmbedRecordWithMedia: &bsky.EmbedRecordWithMedia{
			LexiconTypeID: "app.bsky.embed.recordWithMedia",
			Record:        embedRecord,
			Media:         &bsky.EmbedRecordWithMedia_Media{EmbedImages: embedImages},
		}}
	case embedImages != nil:
		return &bsky.FeedPost_Embed{EmbedImages: embedImages}
	case embedRecord != nil:
		return &bsky.FeedPost_Embed{EmbedRecord: embedRecord}
	}
	return nil
}
