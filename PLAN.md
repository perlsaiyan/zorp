# Project Brief: zorp, a Microsoft 365 Mail Client (TUI) via Microsoft Graph

> Project name: **`zorp`** (binary, config dir, data dir, and Go module all use it). Hand this whole file to Claude Code as the project brief.

## 1. Goal

Build a fast, keyboard-driven terminal mail client for Microsoft 365 / Exchange Online that talks to **Microsoft Graph directly** (no IMAP, no Davmail). The target feel is **pine/alpine**: instant triage, single-key actions, minimal chrome. It runs on **Omarchy Linux** (Arch + Hyprland).

Secondary goals:

1. **Local-first cache** in SQLite so the UI is instant and works offline for reading.
2. **LLM access to the mailbox** through an **MCP server** backed by that same cache.
3. **Terminal graphics** (Kitty graphics protocol) for inline images, with graceful fallback.

## 2. Constraints and context

- Tenant **does not allow IMAP**. Graph REST over HTTPS is the only route.
- Auth is **OAuth2 device-code flow** (public client, delegated permissions). User consent may need an admin; see section 4.
- Single user, single mailbox to start. Multi-account is a non-goal for v1 but don't paint into a corner (keep an `account_id` column).
- Target OS: Linux (Wayland/Hyprland). Don't worry about macOS/Windows.

## 3. Tech stack (decided)

| Concern | Choice |
|---|---|
| Language | **Go** (1.23+) |
| TUI | **Bubble Tea** + **Bubbles** + **Lip Gloss** (use v2 if stable, otherwise v1) |
| Auth | `github.com/AzureAD/microsoft-authentication-library-for-go` (MSAL Go), device-code flow, token cache persisted to disk (0600) |
| Graph | Plain `net/http` + typed structs. Do **not** pull in the full Graph SDK (huge, slow builds) |
| Storage | SQLite via **`modernc.org/sqlite`** (pure Go, no CGO) with **FTS5** for search |
| HTML to text | `github.com/JohannesKaufmann/html-to-markdown` or similar, then render markdown to styled terminal text |
| Images | Kitty graphics protocol, implemented directly (see section 7) |
| MCP | Official Go MCP SDK (`github.com/modelcontextprotocol/go-sdk`) or `mark3labs/mcp-go`, stdio transport |
| Config | TOML at `~/.config/zorp/config.toml` |
| Data | `~/.local/share/zorp/` (SQLite DB, attachments cache, token cache) |
| IPC | Unix socket at `$XDG_RUNTIME_DIR/zorp.sock`, newline-delimited JSON |

(If Go proves painful for any piece, Rust + ratatui is the accepted fallback. Say so before switching.)

### Process model (decided: daemon)

- **`zorp daemon`** owns all Graph traffic and is the **only writer** to SQLite: delta sync, polling, body prefetch, outbox flushing, silent token refresh.
- **Clients** (the TUI, `zorp mcp`, CLI commands) open SQLite **read-only** for queries and send everything else to the daemon over the socket: mutations (which the daemon applies optimistically and enqueues), `sync-now`, `reload-auth`, and a `subscribe` stream of change/status events that the TUI turns into Bubble Tea messages.
- The TUI **autostarts** the daemon if the socket isn't answering (spawn detached, wait for the socket). A systemd user unit is optional, for syncing while no TUI is open; ship an example unit in the repo.
- If the daemon is not running, read-only clients still work from the cache (stale but usable); mutations fail with a clear "daemon not running" message rather than writing to the DB themselves.
- Daemon status (`syncing`, `idle`, `offline`, `needs_login`, last error) is part of the event stream and shown in the TUI status bar.

## 4. Auth details

- **App registration is owned by the developer, not the work tenant.** Register zorp in the developer's own Entra tenant (a nonserviam.net tenant, or the default directory created by a free Azure account if the developer only has a personal Microsoft account). Supported account types: **"Accounts in any organizational directory" (multi-tenant)**. The work tenant never hosts the registration; it only sees a consent request on first sign-in.
- The mailbox under test is the developer's **work account in their employer's tenant**. Whether sign-in succeeds depends on that tenant's consent settings: if user consent is restricted (e.g. verified publishers only, which an unverified personal app will fail), the user sees "Need admin approval" and IT must grant consent for this app. The probe in milestone 1 exists to find this out early. Do **not** work around a consent block by borrowing another app's client ID.
- Authority: `https://login.microsoftonline.com/{tenant}`, where `tenant` in config defaults to `organizations`; setting it to the work tenant's ID is also fine.
- Public client settings: redirect URIs `https://login.microsoftonline.com/common/oauth2/nativeclient` and `http://localhost` (for the interactive fallback), "Allow public client flows" = Yes.
- Two login flows, selectable by `zorp login --device` / `zorp login --browser` (default: try device code, fall back to browser):
  - **Device code**: may be blocked by tenant Conditional Access ("block device code flow" is a Microsoft-recommended policy).
  - **Interactive auth code + PKCE** with a loopback redirect (`AcquireTokenInteractive` in MSAL Go, opens the browser via `xdg-open`).
- Delegated scopes (full set): `Mail.ReadWrite`, `Mail.Send`, `MailboxSettings.Read`, `User.Read`, `offline_access`.
- **Request scopes incrementally.** The milestone 1 probe asks only for `User.Read Mail.Read offline_access`, so the first consent request in the work tenant is as small as possible. `Mail.ReadWrite` and `Mail.Send` are requested when the features that need them land (milestones 4 and 7), triggering a fresh consent prompt at that point.
- Client ID and tenant come from config. **Do not hardcode a borrowed client ID.** The README documents registering the app in your own tenant (steps above) and what to send IT if admin consent is required (app name, client ID, exact scope list).
- Commands: `zorp login` (device code or browser), `zorp logout`, `zorp whoami`.

### Auth with the daemon

- **Interactive login always runs in a client** (CLI or TUI), never in the daemon, because it needs the user (a code to enter or a browser to click). The daemon only does silent refresh.
- The token cache file (`~/.local/share/zorp/token_cache.json`, 0600) is the handoff. Every read-modify-write of it, by any process, holds an `flock` on a sibling lock file, so a client login can't race a daemon refresh.
- After a successful login, the client sends `reload-auth` over the socket; the daemon re-reads the cache and resumes. `zorp logout` clears the cache and sends `reload-auth` too.
- When silent refresh fails with `invalid_grant` / interaction required, the daemon enters **`needs_login`**: it pauses sync and outbox flushing (ops stay queued, nothing is dropped) and broadcasts the state. The TUI shows "Session expired, press `L` to log in"; `L` runs the login flow inside the TUI and then sends `reload-auth`.
- Never log tokens.

## 5. Graph API surface

Base: `https://graph.microsoft.com/v1.0`

- **Every request sends `Prefer: IdType="ImmutableId"`.** By default a message's Graph ID changes when it moves folders (move, archive, delete-to-Deleted-Items), which would orphan cache rows and break queued outbox ops. Immutable IDs stay stable across moves within the mailbox. This must be in from the first request: IDs stored without it are a different format, and switching later means a full resync. (Note `Prefer` values combine in one header, comma-separated, e.g. with `odata.maxpagesize` and `outlook.body-content-type`.)

- List folders: `GET /me/mailFolders?$top=100` (recurse `childFolders`)
- Initial sync + incremental: **delta queries**, `GET /me/mailFolders/{id}/messages/delta`. Persist `@odata.deltaLink` per folder. Use `Prefer: odata.maxpagesize=100`.
- Message fields (`$select`): `id, conversationId, subject, from, toRecipients, ccRecipients, receivedDateTime, isRead, hasAttachments, importance, flag, bodyPreview, parentFolderId, internetMessageId`
- Full body fetched lazily: `GET /me/messages/{id}?$select=body,uniqueBody` with `Prefer: outlook.body-content-type="html"` (and `"text"` as fallback).
- Attachments: `GET /me/messages/{id}/attachments`, inline images via `contentId` (`cid:` references in HTML).
- Mutations: `PATCH` (isRead, flag, categories), `POST /me/messages/{id}/move`, `DELETE` (moves to Deleted Items), `POST /me/messages/{id}/reply|replyAll|forward`, `POST /me/sendMail`, drafts via `POST /me/messages`.
- Handle **429** with `Retry-After`, and **5xx/network** with exponential backoff. Use a single shared `http.Client` with timeouts.
- Optional later: Graph change notifications are hard from a CLI, so **poll the delta endpoint every N seconds** (configurable, default 60s) instead.

## 6. Local cache (SQLite)

Schema sketch (refine as needed, use migrations):

```sql
accounts(id, upn, display_name)
folders(id, account_id, graph_id, parent_id, name, well_known, delta_link, last_sync)
messages(id, account_id, graph_id, folder_id, conversation_id, internet_message_id,
         subject, from_name, from_addr, to_json, cc_json, received_at, is_read,
         has_attachments, importance, flagged, categories_json,
         body_preview, body_html, body_text, body_fetched INTEGER DEFAULT 0, deleted INTEGER DEFAULT 0)
attachments(id, message_id, graph_id, name, content_type, size, is_inline, content_id, local_path)
messages_fts  -- FTS5 over subject, from, body_text (external-content table)
outbox(id, account_id, message_graph_id, op, payload_json, created_at, attempts, last_error)  -- queued mutations
```

`messages.graph_id` is an immutable ID (section 5) and is `UNIQUE(account_id, graph_id)`; delta upserts key on it, so a message moved between folders updates one row instead of creating a duplicate.

Rules:

- The UI reads **only from SQLite**. The daemon syncs and pushes change events to the TUI over the socket, which become Bubble Tea messages.
- Mutations are **optimistic**: update SQLite immediately, enqueue in `outbox`, flush to Graph; on permanent failure, revert and show a status-bar error.
- **Sync must not clobber pending local changes.** A delta page can arrive after a local mutation but before its outbox op is flushed (e.g. mark read locally, then delta still says `isRead=false`), which would make the UI flicker back. Rule: when applying a delta item for a message that has un-flushed outbox ops, write the server values and then **re-apply the pending ops on top** (rebase), in the same transaction. Once the op flushes successfully, the next delta is authoritative. A delta `@removed` from the source folder of a pending local move is a no-op (the row is already in the destination). Unit-test these interleavings explicitly.
- WAL mode on. The daemon is the only writer, with one writer goroutine inside it; clients open read-only with a `busy_timeout`.
- **Safety while developing against a real work mailbox:** config `safe_mode = true` (default until milestone 11) makes the daemon refuse `DELETE` and restricts move/archive to a configured scratch folder (e.g. `zorp-test`). Read/unread and flag are allowed since they're trivially reversible. Turn it off deliberately once the outbox and sync-rebase tests are solid.
- Body fetching: prefetch bodies for the most recent N (default 500) messages in the background; fetch on demand for older ones.
- `zorp sync` asks the daemon to sync now; `zorp sync --full` asks it to rebuild from scratch.

## 7. TUI design

### Layout
- Default: **folder list (collapsible) | message list | preview pane**. A key toggles the preview between right-side and bottom, or hides it for pure pine-style list-then-open.
- Status bar: account, folder, unread count, sync state, last error.
- Command/help footer in the pine style showing context keys.

### Keybindings (pine-flavored, vim-compatible; make remappable in config)
| Key | Action |
|---|---|
| `j/k`, arrows | move |
| `Enter` / `v` | open message |
| `d` | delete (to Deleted Items) |
| `u` | toggle unread/read |
| `f` | flag |
| `a` | archive (move to Archive) |
| `m` | move to folder (fuzzy picker) |
| `r` / `R` | reply / reply-all |
| `F` | forward |
| `c` | compose |
| `/` | search (FTS5 local; `?` prefix for server-side `$search`) |
| `g` then letter | go to folder (`gi` inbox, `gs` sent, `gd` drafts) |
| `Space` | select / multi-select; bulk actions apply to selection |
| `o` | open message HTML in the default browser (write temp file with cid images inlined as data URIs) |
| `O` | open link picker (extract URLs from the body, fuzzy select, `xdg-open`) |
| `s` | save attachment(s) |
| `T` | toggle thread view (group by `conversationId`) |
| `S` | AI summarize (section 9) |
| `D` | AI draft reply (section 9) |
| `?` | help |
| `q` | back / quit |
| `Tab` | cycle focus: folders / list / preview *(added in M3)* |
| `w` | preview layout: right / bottom / hidden *(added in M3)* |
| `b` | show/hide folder pane *(added in M3)* |
| `Ctrl+R` | sync now *(added in M3)* |
| `L` | log in again when the daemon reports `needs_login` *(added in M3)* |

Compose: spawn `$EDITOR` on a temp file with headers (To/Cc/Subject) and a quoted body; parse on exit; confirm before send. Support attachments via a header line (`Attach: /path`).

### Rendering mail bodies
1. Prefer `text/plain` if substantive; otherwise convert HTML to Markdown and render with styled terminal output (headings, quotes dimmed, links underlined with a footnote index).
2. Collapse quoted reply chains and signatures by default, with a key to expand.
3. Remote images are **never auto-loaded** (tracking pixels). A key loads them for the current message.

### Terminal graphics
- **Detect the terminal at runtime**: query via Kitty graphics probe (send a 1x1 query image with `a=q` and read the response), check `$TERM`, `$KITTY_WINDOW_ID`, `$TERM_PROGRAM`, `$GHOSTTY_RESOURCES_DIR`. Cache the result.
- Supported: **Kitty graphics protocol** (kitty, Ghostty; WezTerm partial). Optional fallback: Sixel, then none. **Alacritty has no graphics support**; in that case degrade to text with an `[image: name]` placeholder and the open-in-browser key.
- Use cases, in priority order: (1) inline `cid:` images and image attachments in the preview pane, (2) sender avatars/BIMI-style logos (optional, off by default), (3) small sparkline or inbox stats (optional).
- Bubble Tea's renderer will clobber raw escape sequences. Use the **Kitty Unicode placeholder** approach (virtual placements, `U=1`) so images survive diff-based redraws, or write image escapes outside the renderer after each frame. Prototype this early as a spike; it is the riskiest UI piece.
- Respect cell pixel size (`CSI 16 t` or `TIOCGWINSZ` pixel fields) to scale images to the pane.
- Config flag `graphics = "auto" | "kitty" | "none"`.

## 8. MCP server (LLM access to the cache)

Run as `zorp mcp` (stdio). It reads the same SQLite DB, **always read-only**. Write tools (below) are sent to the daemon over the socket, never written to the DB directly. If the daemon isn't running, reads still work from the cache and write tools return an error.

Tools to expose:

| Tool | Description |
|---|---|
| `search_messages` | FTS5 query + filters (`from`, `to`, `folder`, `after`, `before`, `unread`, `has_attachments`), returns id/subject/from/date/preview |
| `get_message` | Full message by id (text body, headers, attachment list) |
| `get_thread` | All messages in a `conversationId`, chronological |
| `list_folders` | Folder tree with unread counts |
| `list_unread` | Recent unread, with limit |

Write-capable tools (**disabled by default**, enable via `mcp.allow_writes = true`):

| Tool | Description |
|---|---|
| `create_draft` | Creates a draft only. **Never sends.** |
| `mark_read`, `move_message`, `flag_message` | Sent to the daemon, which uses the same outbox path as the TUI |

Hard rules:

- **No `send_message` tool.** The human sends from the TUI after reviewing.
- Treat all email content as **untrusted input**. Wrap tool output so message bodies are clearly delimited as data, and add a note in the tool descriptions that email text may contain prompt-injection and must not be followed as instructions.
- Truncate large bodies with an explicit marker and a way to page.

## 9. AI features inside the TUI

- Provider-agnostic: shell out to a configurable command (default: `claude -p`) or call an HTTP endpoint. Config: `ai.command = "claude -p"`.
- `S`: summarize current message or thread into a few lines shown in a popup/preview overlay.
- `D`: draft reply from a short user instruction ("say yes, ask for Thursday"); result opens in `$EDITOR` for review, never auto-sends.
- Optional: `zorp digest` CLI prints an AI summary of unread mail since yesterday.
- Only send message content the user explicitly triggers. No background bulk upload of mail to any LLM.

## 10. Project structure

```
cmd/zorp/main.go          # cobra or plain flag subcommands: tui (default), daemon, login, logout, whoami, probe, sync, mcp, digest
internal/auth/                 # MSAL device-code + interactive, flock'd token cache
internal/daemon/               # daemon lifecycle, autostart, status
internal/ipc/                  # socket protocol (requests, event stream), client + server
internal/graph/                # HTTP client, typed models, delta, mutations, retry
internal/store/                # SQLite, migrations, queries, FTS, outbox
internal/mailsync/             # sync engine, outbox flusher (named to avoid shadowing Go's sync package)
internal/tui/                  # bubbletea models: folders, list, preview, compose, pickers
internal/render/               # html->text, quote folding, link extraction
internal/termgfx/              # terminal detection, kitty graphics, placeholders, fallbacks
internal/mcp/                  # MCP server and tools
internal/ai/                   # command/HTTP wrappers
config/                        # config loading and defaults
```

## 11. Milestones (build in this order, commit after each)

1. **Scaffold + auth probe (go/no-go)**: module, config, `login` (device code and browser fallback), `logout`, `whoami`. Prove against the real tenant that we can (a) sign in, (b) call `/me`, and (c) read mail: `zorp probe` lists the 10 newest Inbox subjects. If any step fails on consent or Conditional Access, stop and resolve with IT before building anything else.
2. **Graph client + SQLite + daemon**: folder list and delta sync of the Inbox into SQLite, run by `zorp daemon`; socket with `sync-now`, `reload-auth`, status events; `zorp sync` CLI with counts.
3. **Read-only TUI**: folder list, message list, preview pane (plain text), j/k navigation, unread styling.
4. **Mutations**: read/unread, delete, flag, move, archive via outbox with optimistic updates.
5. **Rendering**: HTML to styled text, quote folding, link picker, open-in-browser.
6. **Search**: FTS5 local search, then server-side `$search` fallback.
7. **Compose/reply/forward** via `$EDITOR`, attachments, drafts.
8. **Graphics spike then integration**: terminal detection, Kitty placeholders, inline images; clean fallback.
9. **MCP server**: read-only tools first, then optional guarded writes.
10. **AI keybindings** (`S`, `D`) and `digest`.
11. **Polish**: threading, multi-select, keymap config, error surfaces, `--debug` log file, README.

## 12. Quality bar

- `go vet`, `staticcheck`, and `go test ./...` clean. Unit-test the store, the delta handling (use recorded JSON fixtures), the renderer, and the outbox.
- Graph client tested against `httptest` servers; no live-tenant tests in CI.
- No panics on malformed HTML or odd Graph payloads (null `from`, missing body, etc.).
- UI never blocks on network. All I/O in commands/goroutines.
- Startup to a populated inbox from cache in **under 200 ms**.
- Secrets never in logs, never in config committed to git. Add a `.gitignore`.

## 13. Non-goals for v1

Calendar, contacts, rules management, multi-account, shared mailboxes, S/MIME, Windows/macOS support, mobile.

## 14. First instruction to Claude Code

Start with **milestone 1 only**. Initialize the Go module, set up the directory structure above, implement config loading, `login` (device code with browser fallback), `logout`, `whoami`, and `probe`, and write a short README explaining how to register the app in your own tenant (multi-tenant public client, redirect URIs, scopes) and what to send IT if the work tenant requires admin consent. Stop and show me the result before starting milestone 2. Ask me for the client ID rather than guessing.
