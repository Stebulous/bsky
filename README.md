# bsky

bluesky CLI client written in Go

## Usage

```
NAME:
   bsky - bsky

USAGE:
   bsky [global options] command [command options]

VERSION:
   0.0.67

DESCRIPTION:
   A cli application for bluesky

COMMANDS:
   show-profile         Show profile
   update-profile       Update profile
   show-session         Show session
   timeline, tl         Show timeline
   stream               Show timeline as stream
   thread               Show thread
   post                 Post new text
   vote                 Vote the post
   votes                Show votes of the post
   repost               Repost the post
   reposts              Show reposts of the post
   follow               Follow the handle
   unfollow             Unfollow the handle
   follows              Show follows
   followers            Show followers
   block                Block the handle
   unblock              Unblock the handle
   blocks               Show blocks
   delete               Delete the note
   search               Search Bluesky
   login                Login the social
   notification, notif  Show notifications
   invite-codes         Show invite codes
   list-app-passwords   Show App-passwords
   add-app-password     Add App-password
   revoke-app-password  Revoke App-password
   mcp                  Start MCP server
   help, h              Shows a list of commands or help for one command

GLOBAL OPTIONS:
   -a value       profile name
   -V             verbose (default: false)
   --help, -h     show help
   --version, -v  print the version
```

```
$ bsky login [handle] [password]
$ bsky timeline
```

```
$ bsky post -image ~/pizza.jpg 'I love 🍕'
```

```
$ bsky vote at://did:plc:xxxxxxxxxxxxxxxxxxxxxxxx/app.bsky.feed.post/yyyyyyyyyyyyy
$ bsky repost at://did:plc:xxxxxxxxxxxxxxxxxxxxxxxx/app.bsky.feed.post/yyyyyyyyyyyyy
```

### Extended Usage Information

Individual commands have their own help texts. Call via `-h` / `--help` and the name of the command.

### JSON Output

The output for most commands can be formatted as JSON via `--json`. See Extended Usage Information for the individual commands that support JSON output.

## Installation

Download binary from Release page.

Or install with go install command.
```
go install github.com/mattn/bsky@latest
```

### MCP Server

`bsky mcp` starts an MCP (Model Context Protocol) server over stdio. This allows AI assistants like Claude to interact with Bluesky directly.

Available tools:

| Tool | Description |
|------|-------------|
| `bluesky_timeline` | Show timeline posts |
| `bluesky_post` | Create a new post, optionally with images, a reply or a quote |
| `bluesky_search` | Search posts |
| `bluesky_show_profile` | Show a user profile |
| `bluesky_search_actors` | Search for users |
| `bluesky_thread` | Show a post thread |
| `bluesky_notification` | Show notifications |
| `bluesky_like` | Like a post |
| `bluesky_repost` | Repost a post |
| `bluesky_follow` | Follow a user |
| `bluesky_update_profile` | Update the display name, description, website, avatar or banner |

#### Images in posts

`bluesky_post` takes an optional `images` array with up to 4 items. Each item has `alt` (required alt text) and exactly one of `base64` or `path`:

```json
{
  "text": "New release https://example.com",
  "reply": "https://bsky.app/profile/alice.example/post/3kabc",
  "images": [
    {"base64": "iVBORw0KGgo...", "alt": "Screenshot of the settings page"}
  ]
}
```

- Images must be static PNG, JPEG or WebP, up to 1,000,000 bytes each. The type is detected from the content and the aspect ratio is set automatically. Oversized images are rejected; they are not recompressed.
- `path` reads a file on the machine that runs the server. Paths are disabled by default. To enable them, start the server with `bsky mcp --media-dir DIR` (or set `BSKY_MCP_MEDIA_DIR`); only regular files inside `DIR` can be read.
- Images work with `reply` and `quote`. A quote with images uses an `app.bsky.embed.recordWithMedia` embed.
- `reply` and `quote` accept an `at://` URI or a `https://bsky.app/profile/.../post/...` URL.
- The result contains the post `uri`, `cid` and bsky.app `url`.
- Set `dryRun` to validate the post and see the record without uploading or posting.

#### Profile updates

`bluesky_update_profile` requires `expectedDid`, the DID of the account you intend to edit; the call fails if the authenticated account is different. Optional fields are `displayName`, `description`, `website` (absolute `https://` URL), `avatarBase64` and `bannerBase64` (static PNG or JPEG, up to 1,000,000 bytes). An empty string clears a text field.

- Omitted fields and all other record fields (for example `pinnedPost` or self-labels) are kept unchanged.
- The write uses `swapRecord`, so a profile edit made by another client after the read makes the call fail instead of overwriting it.
- The result contains the prior record and its CID, which you can use to roll back.
- Set `dryRun` to see the proposed record without uploading or writing.

Example configuration for Claude Code (`~/.claude/settings.json`):

```json
{
  "mcpServers": {
    "bsky": {
      "command": "bsky",
      "args": ["mcp"]
    }
  }
}
```

### To enable Autocomplete

Download the correct file from `/scripts` directory and add the following line to your shell configuration file.

ZSH:
```sh
# Add the following line to your .zshrc
source /path/to/autocomplete.zsh
```

Bash:
```bash
# Add the following line to your .bashrc
source /path/to/autocomplete.sh
```

PowerShell:
```powershell
# Add the following line to your $PROFILE
/path/to/autocomplete.ps1
```

## License

MIT

## Author

Yasuhiro Matsumoto (a.k.a. mattn)
