package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/api/bsky"
	lexutil "github.com/bluesky-social/indigo/lex/util"
	"github.com/bluesky-social/indigo/xrpc"
	"github.com/mark3labs/mcp-go/mcp"
)

type postImageArg struct {
	Path   string `json:"path"`
	URL    string `json:"url"`
	Base64 string `json:"base64"`
	Alt    string `json:"alt"`
}

type postArgs struct {
	Text   string         `json:"text"`
	Reply  string         `json:"reply"`
	Quote  string         `json:"quote"`
	Images []postImageArg `json:"images"`
	DryRun bool           `json:"dryRun"`
}

var postImageItemSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"alt"},
	"properties": map[string]any{
		"base64": map[string]any{
			"type":        "string",
			"description": "Base64 image data (an optional data: URL prefix is accepted). Give exactly one of base64, url or path.",
		},
		"url": map[string]any{
			"type":        "string",
			"description": "Public https:// URL that the MCP server downloads the image from, for example a raw.githubusercontent.com file. Disabled unless the server runs with --allow-image-urls.",
		},
		"path": map[string]any{
			"type":        "string",
			"description": "Image file path on the MCP server, inside its --media-dir. Paths are disabled when the server has no media directory.",
		},
		"alt": map[string]any{
			"type":        "string",
			"minLength":   1,
			"description": "Alt text that describes the image for screen-reader users. Required.",
		},
	},
}

func newPostTool() mcp.Tool {
	return mcp.NewTool("bluesky_post",
		mcp.WithDescription("Create a new post on Bluesky, optionally with up to 4 images. "+
			"Images are static PNG, JPEG or WebP, up to 1,000,000 bytes each, and every image needs alt text. "+
			"The type is detected from the content and the aspect ratio is set automatically. Oversized images are rejected, not recompressed. "+
			"Links, @mentions and #tags in text become clickable. Images work with reply and with quote. "+
			"Returns the post uri, cid and bsky.app url. Set dryRun to validate everything without uploading or posting."),
		mcp.WithString("text",
			mcp.Description("Text content of the post (max 300 characters). May be empty when images are attached."),
			mcp.Required(),
		),
		mcp.WithString("reply",
			mcp.Description("URI of the post to reply to (at:// URI or https://bsky.app post URL)"),
		),
		mcp.WithString("quote",
			mcp.Description("URI of the post to quote (at:// URI or https://bsky.app post URL)"),
		),
		mcp.WithArray("images",
			mcp.Description("Images to attach, in display order. Each item has alt and exactly one of base64, url or path."),
			mcp.MaxItems(maxPostImages),
			mcp.Items(postImageItemSchema),
		),
		mcp.WithBoolean("dryRun",
			mcp.Description("Validate the post and images and return the record that would be created, without uploading or posting."),
		),
		mcp.WithSchemaAdditionalProperties(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
}

// bindStrict decodes the tool arguments into target and rejects unknown
// arguments, because clients do not always enforce additionalProperties.
func bindStrict(request mcp.CallToolRequest, target any) error {
	b, err := json.Marshal(request.GetArguments())
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

type preparedImage struct {
	data []byte
	info imageInfo
	alt  string
}

// preparePostImages loads and validates all images before anything is
// uploaded, so that one bad image does not leave a partial post.
func preparePostImages(ctx context.Context, media *mediaDir, urls *imageFetcher, args []postImageArg) ([]preparedImage, error) {
	if len(args) > maxPostImages {
		return nil, fmt.Errorf("%d images given; a post can have at most %d", len(args), maxPostImages)
	}
	var images []preparedImage
	for i, a := range args {
		alt := strings.TrimSpace(a.Alt)
		if alt == "" {
			return nil, fmt.Errorf("images[%d]: alt text is required", i)
		}
		var b []byte
		var err error
		sources := 0
		for _, s := range []string{a.Base64, a.URL, a.Path} {
			if s != "" {
				sources++
			}
		}
		switch {
		case sources > 1:
			err = errors.New("give only one of base64, url or path")
		case a.Base64 != "":
			b, err = decodeBase64Image(a.Base64)
		case a.URL != "":
			b, err = urls.fetch(ctx, a.URL)
		case a.Path != "":
			b, err = media.readImage(a.Path)
		default:
			err = errors.New("base64, url or path is required")
		}
		if err != nil {
			return nil, fmt.Errorf("images[%d]: %w", i, err)
		}
		info, err := inspectImage(b, postImageTypes)
		if err != nil {
			return nil, fmt.Errorf("images[%d]: %w", i, err)
		}
		images = append(images, preparedImage{data: b, info: info, alt: alt})
	}
	return images, nil
}

func buildFacets(ctx context.Context, c lexutil.LexClient, text string) []*bsky.RichtextFacet {
	var facets []*bsky.RichtextFacet
	for _, entry := range extractLinksBytes(text) {
		facets = append(facets, &bsky.RichtextFacet{
			Features: []*bsky.RichtextFacet_Features_Elem{
				{RichtextFacet_Link: &bsky.RichtextFacet_Link{Uri: entry.text}},
			},
			Index: &bsky.RichtextFacet_ByteSlice{ByteStart: entry.start, ByteEnd: entry.end},
		})
	}
	for _, entry := range extractMentionsBytes(text) {
		profile, err := bsky.ActorGetProfile(ctx, c, entry.text)
		if err != nil {
			continue
		}
		facets = append(facets, &bsky.RichtextFacet{
			Features: []*bsky.RichtextFacet_Features_Elem{
				{RichtextFacet_Mention: &bsky.RichtextFacet_Mention{Did: profile.Did}},
			},
			Index: &bsky.RichtextFacet_ByteSlice{ByteStart: entry.start, ByteEnd: entry.end},
		})
	}
	for _, entry := range extractTagsBytes(text) {
		facets = append(facets, &bsky.RichtextFacet{
			Features: []*bsky.RichtextFacet_Features_Elem{
				{RichtextFacet_Tag: &bsky.RichtextFacet_Tag{Tag: entry.text}},
			},
			Index: &bsky.RichtextFacet_ByteSlice{ByteStart: entry.start, ByteEnd: entry.end},
		})
	}
	return facets
}

func createPost(ctx context.Context, c *xrpc.Client, media *mediaDir, urls *imageFetcher, args postArgs) (any, error) {
	if len(args.Text) > maxPostTextBytes {
		return nil, fmt.Errorf("text is %d bytes; the limit is %d bytes", len(args.Text), maxPostTextBytes)
	}
	if strings.TrimSpace(args.Text) == "" && len(args.Images) == 0 {
		return nil, errors.New("text is required unless images are attached")
	}
	images, err := preparePostImages(ctx, media, urls, args.Images)
	if err != nil {
		return nil, err
	}

	post := &bsky.FeedPost{
		LexiconTypeID: "app.bsky.feed.post",
		Text:          args.Text,
		CreatedAt:     time.Now().Local().Format(time.RFC3339),
	}
	if args.Reply != "" {
		parent, parentPost, err := getPost(ctx, c, args.Reply)
		if err != nil {
			return nil, err
		}
		root := parent
		if parentPost.Reply != nil && parentPost.Reply.Root != nil {
			root = &comatproto.RepoStrongRef{Uri: parentPost.Reply.Root.Uri, Cid: parentPost.Reply.Root.Cid}
		}
		post.Reply = &bsky.FeedPost_ReplyRef{Root: root, Parent: parent}
	}
	var quote *comatproto.RepoStrongRef
	if args.Quote != "" {
		if quote, _, err = getPost(ctx, c, args.Quote); err != nil {
			return nil, err
		}
	}
	post.Facets = buildFacets(ctx, c, args.Text)

	var embedImages []*bsky.EmbedImages_Image
	for _, img := range images {
		ei := &bsky.EmbedImages_Image{
			Alt:         img.alt,
			AspectRatio: &bsky.EmbedDefs_AspectRatio{Width: int64(img.info.Width), Height: int64(img.info.Height)},
		}
		if !args.DryRun {
			if ei.Image, err = uploadImageBlob(ctx, c, img.data, img.info.MimeType); err != nil {
				return nil, err
			}
		}
		embedImages = append(embedImages, ei)
	}
	post.Embed = postEmbed(embedImages, quote)

	if args.DryRun {
		infos := make([]imageInfo, len(images))
		for i, img := range images {
			infos[i] = img.info
		}
		return map[string]any{
			"dryRun": true,
			"note":   "Nothing was uploaded or posted. Image blobs are uploaded when dryRun is false.",
			"record": post,
			"images": infos,
		}, nil
	}

	resp, err := comatproto.RepoCreateRecord(ctx, c, &comatproto.RepoCreateRecord_Input{
		Collection: "app.bsky.feed.post",
		Repo:       c.Auth.Did,
		Record:     &lexutil.LexiconTypeDecoder{Val: post},
	})
	if err != nil {
		return nil, fmt.Errorf("cannot create post: %w", err)
	}
	return map[string]any{
		"uri":    resp.Uri,
		"cid":    resp.Cid,
		"url":    postWebURL(resp.Uri),
		"images": len(images),
	}, nil
}
