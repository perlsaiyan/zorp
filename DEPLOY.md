# Deploying zorp on Omarchy (Hyprland + Ghostty)

This is a checklist for setting zorp up on a desktop, written so it can be handed to an assistant. It assumes Omarchy (Arch, Hyprland, mako for notifications) and Ghostty. Ghostty is the terminal to use: it shows zorp's 24-bit colour tints and inline images (Kitty graphics), and it displays terminal notifications.

## 1. Get the binary

Releases are on the private GitHub repo (`perlsaiyan/zorp`). With `gh` signed in to an account that can see it:

```sh
gh release download --repo perlsaiyan/zorp --pattern 'zorp-*-linux-amd64.tar.gz' --pattern SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
tar xzf zorp-*-linux-amd64.tar.gz
cd zorp-*-linux-amd64
install -Dm755 zorp ~/.local/bin/zorp
install -Dm755 contrib/zorp-focus ~/.local/bin/zorp-focus
zorp version
```

Use `linux-arm64` on an ARM machine. The binary is static, with no cgo, so it needs nothing else installed. `~/.local/bin` must be on `PATH`; it is on Omarchy.

To build from source instead: `go build -o ~/.local/bin/zorp ./cmd/zorp` (Go 1.26).

## 2. Ghostty

```sh
command -v ghostty || sudo pacman -S ghostty
```

## 3. Config and sign-in

```sh
mkdir -p ~/.config/zorp
cp contrib/config.example.toml ~/.config/zorp/config.toml
zorp login
```

`zorp login` signs in to the Microsoft 365 account. It uses a device code, falling back to the browser. The app registration's client ID is already in the example config. The token is stored in `~/.local/share/zorp`, readable only by the user.

Guardrails are off by default: zorp can send, move and delete like any mail client. `safe_mode = true` in `[safety]` turns them back on. That's the owner's decision, so don't change `[safety]` unless asked. The README explains every option.

## 4. Daemon as a service

The daemon syncs mail and sends new-mail notifications, so it should run whether or not a zorp window is open:

```sh
install -Dm644 contrib/zorp.service ~/.config/systemd/user/zorp.service
systemctl --user daemon-reload
systemctl --user enable --now zorp
zorp daemon status
journalctl --user -u zorp -n 50
```

If a zorp window is open while you do this, its own daemon may hold the lock. Quit zorp, run `zorp daemon stop`, then restart the service.

## 5. Hotkey and window

`zorp-focus` focuses zorp's window if it's open, or opens zorp in a new Ghostty window with the class `com.zorp.mail`. Bind it in `~/.config/hypr/bindings.conf`, Omarchy's file for user bindings. First check that the key is free with `hyprctl binds`:

```
bindd = SUPER ALT, M, Mail (zorp), exec, zorp-focus
```

Optional window rules, in `~/.config/hypr/hyprland.conf` or a file it sources. The rule syntax has changed between Hyprland releases, so copy the form Omarchy's own rules use (in `~/.local/share/omarchy/default/hypr/`):

```
windowrule = workspace 9, class:^(com\.zorp\.mail)$
# or a floating window:
# windowrule = float, class:^(com\.zorp\.mail)$
# windowrule = size 70% 80%, class:^(com\.zorp\.mail)$
```

Reload with `hyprctl reload`.

## 6. Check it

- `zorp notify-test` should show a mako notification. Clicking it runs `zorp-focus`.
  - If the notification shows but clicking does nothing, mako may not invoke the default action on click. Check `on-button-left=invoke-default-action` in mako's config.
  - If `zorp-focus` can't reach Hyprland from the service, run `systemctl --user import-environment WAYLAND_DISPLAY HYPRLAND_INSTANCE_SIGNATURE`. Omarchy's uwsm session normally exports these already.
- In zorp, the cursor row and selected rows (Space) should be soft colour tints, not reverse video. If they aren't, set `ui.color = "tint"`.
- A message with inline images (charts in a report) should show them in the preview.

## Upgrading

Download and install the new release over the old binary as in step 1, then:

```sh
systemctl --user restart zorp
```

Then restart any open zorp windows. The cache schema has to match the daemon's, and the daemon migrates it on start.

## Uninstalling

```sh
systemctl --user disable --now zorp
rm ~/.local/bin/zorp ~/.local/bin/zorp-focus ~/.config/systemd/user/zorp.service
rm -r ~/.local/share/zorp ~/.config/zorp   # cache, sign-in and config
```
