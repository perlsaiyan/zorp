# zorp

A fast, keyboard-driven terminal mail client for Microsoft 365, talking to Microsoft Graph directly (no IMAP). See [PLAN.md](PLAN.md) for the full design.

**Status:** milestone 2 (sync). Sign-in, the sync daemon and the local cache work; there is no TUI yet.

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

5. Under **API permissions → Add a permission → Microsoft Graph → Delegated permissions**, add **User.Read**, **Mail.Read** and **offline_access**. Don't click "Grant admin consent" here; that only applies to your own tenant.

zorp requests scopes at sign-in (incremental consent), so the list in step 5 isn't strictly needed when you can consent yourself. It matters when your organization requires an admin: their "Grant admin consent" button only grants the permissions listed on the registration. `Mail.ReadWrite`, `Mail.Send` and `MailboxSettings.Read` will be added to that list, and requested, by later milestones.

## Signing in

```sh
zorp login            # device code; falls back to the browser if that fails
zorp login --browser  # browser sign-in (auth code + PKCE via http://localhost)
zorp probe            # go/no-go: signed in? /me readable? Inbox readable?
zorp whoami
zorp logout
```

Tokens are cached in `~/.local/share/zorp/token_cache.json` (mode 0600) and are never logged.

## Syncing

A background daemon keeps a local SQLite cache (`~/.local/share/zorp/zorp.db`) in sync with Graph using delta queries, polling every 60 seconds. Commands that need it start it automatically; it logs to `~/.local/share/zorp/daemon.log`.

```sh
zorp sync             # sync now and print counts
zorp sync --full      # discard the cache and rebuild it
zorp daemon status    # state, pid, last sync, last error
zorp daemon stop
zorp daemon           # run in the foreground, logging to stderr
```

Sync settings in `config.toml` (defaults shown):

```toml
[sync]
interval = "60s"        # delta poll interval
initial_days = 90       # first sync only fetches this much history; 0 = everything
folders = ["inbox"]     # well-known names (inbox, sentitems, drafts, archive, ...) or folder names
```

## Admin consent

The first time you sign in with a work account, your organization decides whether you may consent to zorp yourself. If you may, the sign-in shows a **Permissions requested** screen; click **Accept** and you're done. Many tenants only allow user consent for apps from verified publishers, which a personal registration is not. If you see **"Need admin approval"** (or `zorp login` reports `AADSTS65001`, `AADSTS90094` or `AADSTS90095`), ask IT to grant consent. If the screen offers **Request approval**, use it; admins see it under **Enterprise applications → Admin consent requests**. Otherwise, send them:

- **App name:** zorp
- **Application (client) ID:** your `client_id`
- **Type:** public client (no secret), delegated permissions only, signs in as you
- **Delegated permissions:** `User.Read`, `Mail.Read`, `offline_access` now; later `Mail.ReadWrite`, `Mail.Send`, `MailboxSettings.Read`
- **How to grant:** Entra admin center → Enterprise applications → zorp → Permissions → Grant admin consent (the app appears there after your first sign-in attempt), or open `https://login.microsoftonline.com/<tenant-id>/adminconsent?client_id=<client_id>` as an admin

If sign-in is blocked by Conditional Access (`AADSTS53003`), try `zorp login --browser`; device code flow is commonly blocked while browser sign-in is allowed.
