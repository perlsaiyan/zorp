# zorp

A fast, keyboard-driven terminal mail client for Microsoft 365, talking to Microsoft Graph directly (no IMAP). See [PLAN.md](PLAN.md) for the full design.

**Status:** milestone 5 (rendering). HTML mail renders as styled text with link footnotes and folded reply history; read/unread, flag, archive and move work, in safe mode by default (see below). Search and compose come next.

## Build

```sh
go build -o zorp ./cmd/zorp
```

## App registration

zorp needs an Entra ID app registration to sign in. It is a public client (no secret), and it does **not** need to live in your employer's tenant: register it in a tenant you own, and make it multi-tenant so work accounts from any organization can sign in.

1. Sign in to <https://entra.microsoft.com> with your own account. If you only have a personal Microsoft account, sign up for a free Azure account first; that creates a tenant ("Default Directory") for you.
2. **Applications → App registrations → New registration**
   - Name: `zorp`
   - Supported account types: **Accounts in any organizational directory (Multitenant)**
   - Redirect URI: platform **Public client/native (mobile & desktop)**, value `https://login.microsoftonline.com/common/oauth2/nativeclient`
3. In the new app, under **Authentication**:
   - Add a second redirect URI under the same platform: `http://localhost`
   - Set **Allow public client flows** to **Yes**, then save.
4. Copy the **Application (client) ID** from the Overview page into `~/.config/zorp/config.toml`:

```toml
[auth]
client_id = "00000000-0000-0000-0000-000000000000"
tenant = "organizations"   # any work/school account; or your org's tenant ID
```

5. Under **API permissions → Add a permission → Microsoft Graph → Delegated permissions**, add **User.Read**, **Mail.Read**, **Mail.ReadWrite**, **Mail.Send** and **offline_access**. Don't click "Grant admin consent" here; that only applies to your own tenant.

zorp requests scopes at sign-in (incremental consent), so the list in step 5 isn't strictly needed when you can consent yourself. It matters when your organization requires an admin: their "Grant admin consent" button only grants the permissions listed on the registration. `Mail.Send` and `MailboxSettings.Read` will be added to that list, and requested, by later milestones.

## Signing in

```sh
zorp login            # device code; falls back to the browser if that fails
zorp login --browser  # browser sign-in (auth code + PKCE via http://localhost)
zorp probe            # go/no-go: signed in? /me readable? Inbox readable?
zorp whoami
zorp logout
```

Tokens are cached in `~/.local/share/zorp/token_cache.json` (mode 0600) and are never logged.

## Using it

Run `zorp` to open the mail client. The first run syncs before opening. Press `?` inside for all keys; the essentials:

| Key | Action |
|---|---|
| `j` / `k` | move (or scroll, in the preview and reader) |
| `Enter` / `v` | read the message full screen (`n`/`p` next/previous, `q` back) |
| `Tab` | switch between folders, list and preview |
| `/` | search (see below); `Esc` goes back to the folder |
| `g` then `i`/`s`/`d`/`a` | go to Inbox, Sent, Drafts, Archive |
| `w` | preview on the right, at the bottom, or hidden |
| `b` | show/hide the folder pane |
| `u` | toggle read / unread |
| `f` | toggle flag |
| `d` | archive (or delete, with `keys.delete_archives = false`) |
| `X` | delete (moves to Deleted Items), after confirming |
| `a` | archive |
| `m` | move to a folder (type to filter, Enter to move) |
| `c` | write a new message |
| `r` / `R` | reply / reply to all |
| `F` | forward |
| `e` | edit a draft (in Drafts) |
| `s` | save attachments (to `~/Downloads`; `ui.save_dir`) |
| `z` | show/hide quoted replies and signature |
| `O` | pick a link and open it in the browser |
| `o` | open the message in the browser (inline images shown; remote images and scripts blocked) |
| `+` / `-` | start / stop syncing the folder (in the folder pane: the one under the cursor) |
| `Ctrl+R` | sync now |
| `L` | log in again when the session has expired |
| `q` | back / quit |

With the mouse you can click messages and folders, scroll with the wheel, and drag the pane dividers to resize them; sizes and layout are remembered. Hold Shift to select text, or set `ui.mouse = false` to turn mouse support off.

Mail is shown as text: links become numbered footnotes (security-gateway rewrites like Check Point and Safe Links are unwrapped for display, but the original link is what gets opened), quoted reply history and `-- ` signatures are folded, and images appear as `[image: …]` placeholders. Remote images are never downloaded.

Opening a message (`Enter`) marks it read. Changes show up immediately and are sent to Microsoft in the background; the top bar shows how many are still pending. If Microsoft rejects one, it's undone and the reason appears at the bottom.

Message list markers: `N` unread, `*` flagged, `!` high importance, `+` attachments. Folders that aren't synced are dimmed; add them to `sync.folders` to cache their mail.

## Searching

`/` searches every cached message (subject, sender and body), newest first. All words must match, and each word matches as a prefix (`budg` finds "budget").

| Syntax | Matches |
|---|---|
| `budget "q3 report"` | words and exact phrases |
| `-newsletter` | leaves out messages containing the word |
| `from:alice` `to:bob@example.com` `subject:invoice` | sender (name or address), To/Cc, subject |
| `in:archive` | one folder, by name |
| `is:unread` `is:read` `is:flagged` `has:attachment` | message state |
| `after:2026-01-31` `before:7d` | received date; ages can use `d`, `w`, `m`, `y` |

The local cache only covers `sync.initial_days` of mail. Start a search with `?` to search the whole mailbox on the server, using Outlook's syntax (`?from:alice received<2025-01-01`). When nothing cached matches, zorp searches the server automatically, unless the query uses `in:` or `is:`, which only work locally. Messages found on the server can be read and acted on like any other. They're cached for a week but don't appear in folder lists.

From the shell: `zorp search QUERY` (add `--server` for a server search, `-n 100` for more results).

## Writing mail

`c`, `r`, `R` and `F` open your editor (`compose.editor`, else `$VISUAL`, `$EDITOR`, or `vi`) on a file like this:

```
To: Alice Example <alice@example.com>, bob@example.com
Cc:
Bcc:
Subject: Re: Quarterly numbers
Attach: ~/Documents/report.pdf

Thanks, looks good.

On Mon, 26 Oct 2026 at 09:00, Alice Example <alice@example.com> wrote:
> See attached.
```

- Headers on top, then a blank line, then the message. Separate addresses with commas or semicolons.
- Add one `Attach:` line per file (up to 150 MB each). Forwards and drafts list their existing attachments as `Attached:` lines; delete a line to drop that attachment.
- Replies go to the sender's Reply-To address when it has one. Reply-all copies everyone else and leaves you out.
- Messages are written in **Markdown** and sent formatted (HTML): `**bold**`, lists, tables, `code`, links, headings, `> quotes`. Line breaks are kept as you type them. Raw HTML in the text is dropped. Put `Format: text` in the headers (or set `compose.format = "text"`) to send plain text instead.
- In a Markdown reply or forward, the quoted original below the `<!-- zorp: … -->` line is there to read while you write. It isn't sent; Outlook's formatted copy of the original goes under your message instead, the way Outlook itself replies. Delete that line to send the text below it as written, for example to answer inline between quoted lines.
- Drafts saved from zorp reopen (`e`) exactly as you wrote them. A draft written or changed elsewhere opens as plain text and loses its formatting.
- Replies and forwards thread with the original in Outlook.

When you quit the editor, zorp shows the message for review, formatted as the recipient will see it. Press `y` to send, `d` to save to Drafts, `e` to edit again, or `q` to discard. Quitting the editor without changes discards a new message.

If sending fails partway, the message is kept in Drafts. Open it there with `e` to try again.

```toml
[compose]
signature = "Sam Rivera\nExample Corp"   # added above the quoted text
editor = "nvim"                      # overrides $VISUAL / $EDITOR
format = "markdown"                  # or "text"

[ui]
save_dir = "~/Downloads"             # where s saves attachments
```

## Syncing

A background daemon keeps a local SQLite cache (`~/.local/share/zorp/zorp.db`) in sync with Graph using delta queries, polling every 60 seconds. Commands that need it start it automatically; it logs to `~/.local/share/zorp/daemon.log`.

```sh
zorp sync             # sync now and print counts
zorp sync --full      # discard the cache and rebuild it
zorp daemon start     # start it in the background (zorp and zorp sync do this for you)
zorp daemon status    # state, pid, last sync, last error
zorp daemon stop
zorp daemon           # run in the foreground, logging to stderr (for systemd or debugging)
```

Sync settings in `config.toml` (defaults shown):

```toml
[sync]
interval = "60s"        # delta poll interval
initial_days = 90       # first sync only fetches this much history; 0 = everything
folders = ["inbox", "sentitems", "drafts", "archive"]  # well-known names (inbox, sentitems, drafts, archive, ...) or folder names
prefetch_bodies = 500   # download bodies of the newest N messages in the background; older ones load when opened
```

Folders you turn on or off with `+`/`-` in the TUI override this list; the choice is stored in the cache, not written to `config.toml`.

Other settings:

```toml
[keys]
delete_archives = true  # d archives instead of deleting; X still deletes, after confirming

[ui]
mouse = true            # click, scroll, drag dividers; Shift+drag to select text
```

## Safety guardrails

zorp starts with three guardrails on. The daemon enforces them, so they cover every client, including the MCP server later:

| Guardrail | When on | Setting to lift it |
|---|---|---|
| send | mail can only be sent to yourself and to `send_allow` entries | `allow_send = true` |
| move | moves and archive only go to `scratch_folder` | `allow_move = true` |
| delete | deleting is refused | `allow_delete = true` |

Drafts can always be saved, and read/unread and flag always work. The top bar shows the guardrails in force (`guarded: send move delete`), as does `zorp daemon status`.

`safe_mode` sets the default for all three, and each `allow_*` setting overrides it:

```toml
[safety]
safe_mode = true                  # default; false lifts every guardrail not set below
allow_move = true                 # e.g. archive and move freely...
allow_send = false                # ...but keep sending restricted
send_allow = ["example.com", "friend@example.org"]   # besides yourself
allow_delete = false
scratch_folder = "zorp-test"
```

Restart the daemon (`zorp daemon stop`) after changing these.

## Admin consent

The first time you sign in with a work account, your organization decides whether you may consent to zorp yourself. If you may, the sign-in shows a **Permissions requested** screen; click **Accept** and you're done. Many tenants only allow user consent for apps from verified publishers, which a personal registration is not. If you see **"Need admin approval"** (or `zorp login` reports `AADSTS65001`, `AADSTS90094` or `AADSTS90095`), ask IT to grant consent. If the screen offers **Request approval**, use it; admins see it under **Enterprise applications → Admin consent requests**. Otherwise, send them:

- **App name:** zorp
- **Application (client) ID:** your `client_id`
- **Type:** public client (no secret), delegated permissions only, signs in as you
- **Delegated permissions:** `User.Read`, `Mail.Read`, `Mail.ReadWrite`, `Mail.Send`, `offline_access` now; later `MailboxSettings.Read`
- **How to grant:** Entra admin center → Enterprise applications → zorp → Permissions → Grant admin consent (the app appears there after your first sign-in attempt), or open `https://login.microsoftonline.com/<tenant-id>/adminconsent?client_id=<client_id>` as an admin

If sign-in is blocked by Conditional Access (`AADSTS53003`), try `zorp login --browser`; device code flow is commonly blocked while browser sign-in is allowed.
