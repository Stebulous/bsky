package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	lexutil "github.com/bluesky-social/indigo/lex/util"
	"github.com/bluesky-social/indigo/xrpc"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	profileCollection = "app.bsky.actor.profile"
	// maxDisplayNameBytes and maxDescriptionBytes are the maxLength values of
	// app.bsky.actor.profile. The PDS also enforces the grapheme limits.
	maxDisplayNameBytes = 640
	maxDescriptionBytes = 2560
)

type profileArgs struct {
	ExpectedDid  string  `json:"expectedDid"`
	DisplayName  *string `json:"displayName"`
	Description  *string `json:"description"`
	Website      *string `json:"website"`
	AvatarBase64 *string `json:"avatarBase64"`
	BannerBase64 *string `json:"bannerBase64"`
	DryRun       bool    `json:"dryRun"`
}

func newUpdateProfileTool() mcp.Tool {
	return mcp.NewTool("bluesky_update_profile",
		mcp.WithDescription("Update the authenticated Bluesky profile. Omitted fields and unrelated profile data are preserved; "+
			"an empty string clears displayName, description or website. Images are base64 static PNG or JPEG, up to 1,000,000 bytes each. "+
			"Returns the prior record for rollback. A concurrent profile edit causes failure rather than overwriting it. "+
			"Set dryRun to see the record that would be written without uploading or writing."),
		mcp.WithString("expectedDid",
			mcp.Description("Expected DID of the authenticated account; required to prevent editing the wrong account."),
			mcp.Required(),
		),
		mcp.WithString("displayName", mcp.MaxLength(maxDisplayNameBytes)),
		mcp.WithString("description", mcp.MaxLength(maxDescriptionBytes)),
		mcp.WithString("website", mcp.Description("Absolute HTTPS website URL.")),
		mcp.WithString("avatarBase64", mcp.Description("Base64-encoded static PNG or JPEG avatar.")),
		mcp.WithString("bannerBase64", mcp.Description("Base64-encoded static PNG or JPEG banner.")),
		mcp.WithBoolean("dryRun", mcp.Description("Validate and return the proposed record without uploading images or writing the profile.")),
		mcp.WithSchemaAdditionalProperties(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
}

type profileRecord struct {
	Uri   string                     `json:"uri"`
	Cid   *string                    `json:"cid"`
	Value map[string]json.RawMessage `json:"value"`
}

// getRawProfile reads the profile record as raw JSON, so that fields this
// program does not know (pinnedPost, labels, pronouns, ...) are kept as they
// are. It returns nil when the account has no profile record.
func getRawProfile(ctx context.Context, c lexutil.LexClient, did string) (*profileRecord, error) {
	var out profileRecord
	err := c.LexDo(ctx, lexutil.Query, "", "com.atproto.repo.getRecord", map[string]any{
		"repo": did, "collection": profileCollection, "rkey": "self",
	}, nil, &out)
	if err != nil {
		var xe *xrpc.XRPCError
		if errors.As(err, &xe) && xe.ErrStr == "RecordNotFound" {
			return nil, nil
		}
		return nil, fmt.Errorf("cannot get profile record: %w", err)
	}
	if out.Cid == nil || out.Value == nil {
		return nil, errors.New("cannot get profile record: PDS returned no cid or value")
	}
	return &out, nil
}

func validateWebsite(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fmt.Errorf("website %q must be an absolute https:// URL", s)
	}
	return nil
}

func updateProfile(ctx context.Context, c *xrpc.Client, args profileArgs) (any, error) {
	if !strings.HasPrefix(args.ExpectedDid, "did:") {
		return nil, errors.New("expectedDid must be a DID")
	}
	if args.ExpectedDid != c.Auth.Did {
		return nil, fmt.Errorf("authenticated account is %s, not expectedDid %s; nothing was changed", c.Auth.Did, args.ExpectedDid)
	}

	// Validate everything before the first network write.
	text := map[string]*string{"displayName": args.DisplayName, "description": args.Description, "website": args.Website}
	if args.DisplayName != nil && len(*args.DisplayName) > maxDisplayNameBytes {
		return nil, fmt.Errorf("displayName is %d bytes; the limit is %d", len(*args.DisplayName), maxDisplayNameBytes)
	}
	if args.Description != nil && len(*args.Description) > maxDescriptionBytes {
		return nil, fmt.Errorf("description is %d bytes; the limit is %d", len(*args.Description), maxDescriptionBytes)
	}
	if args.Website != nil && *args.Website != "" {
		if err := validateWebsite(*args.Website); err != nil {
			return nil, err
		}
	}
	type pendingImage struct {
		data []byte
		info imageInfo
	}
	images := map[string]pendingImage{}
	for field, v := range map[string]*string{"avatar": args.AvatarBase64, "banner": args.BannerBase64} {
		if v == nil {
			continue
		}
		b, err := decodeBase64Image(*v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		info, err := inspectImage(b, profileImageTypes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		images[field] = pendingImage{data: b, info: info}
	}
	var changed []string
	for field, v := range text {
		if v != nil {
			changed = append(changed, field)
		}
	}
	for field := range images {
		changed = append(changed, field)
	}
	if len(changed) == 0 {
		return nil, errors.New("no profile field given; nothing to update")
	}
	slices.Sort(changed)

	prior, err := getRawProfile(ctx, c, c.Auth.Did)
	if err != nil {
		return nil, err
	}
	record := map[string]json.RawMessage{}
	if prior != nil {
		maps.Copy(record, prior.Value)
	}
	record["$type"] = json.RawMessage(`"` + profileCollection + `"`)
	for field, v := range text {
		switch {
		case v == nil:
		case *v == "":
			delete(record, field)
		default:
			b, _ := json.Marshal(*v)
			record[field] = b
		}
	}
	for field, img := range images {
		if args.DryRun {
			b, _ := json.Marshal(map[string]any{"$type": "blob", "mimeType": img.info.MimeType, "size": img.info.Size, "pendingUpload": true})
			record[field] = b
			continue
		}
		blob, err := uploadImageBlob(ctx, c, img.data, img.info.MimeType)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		b, err := json.Marshal(blob)
		if err != nil {
			return nil, err
		}
		record[field] = b
	}

	// previousRecord, previousCid and record keep the result shape of the
	// earlier bluesky_update_profile wrapper.
	result := map[string]any{"did": c.Auth.Did, "changed": changed, "previousRecord": nil, "previousCid": nil}
	if prior != nil {
		result["previousRecord"] = prior.Value
		result["previousCid"] = *prior.Cid
	}
	if args.DryRun {
		result["dryRun"] = true
		result["proposedRecord"] = record
		return result, nil
	}

	// swapRecord makes the PDS reject the write when the profile changed
	// after it was read. Without a prior record, createRecord with rkey
	// "self" fails if another client creates the profile first.
	var out struct {
		Uri string `json:"uri"`
		Cid string `json:"cid"`
	}
	input := map[string]any{"repo": c.Auth.Did, "collection": profileCollection, "rkey": "self", "record": record, "validate": true}
	method := "com.atproto.repo.createRecord"
	if prior != nil {
		input["swapRecord"] = *prior.Cid
		method = "com.atproto.repo.putRecord"
	}
	if err := c.LexDo(ctx, lexutil.Procedure, "application/json", method, nil, input, &out); err != nil {
		var xe *xrpc.XRPCError
		if errors.As(err, &xe) && (xe.ErrStr == "InvalidSwap" || prior == nil && strings.Contains(strings.ToLower(xe.Message), "already exists")) {
			return nil, errors.New("the profile changed after it was read; nothing was overwritten. Read the profile again and retry")
		}
		return nil, fmt.Errorf("cannot update profile: %w", err)
	}
	result["uri"] = out.Uri
	result["cid"] = out.Cid
	result["record"] = record
	return result, nil
}
