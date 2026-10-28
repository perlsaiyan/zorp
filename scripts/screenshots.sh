#!/usr/bin/env bash
# Takes the README's screenshots (docs/screenshots/*.png) of zorp running
# on made-up mail (scripts/demo), in a detached tmux.
#
# Needs tmux, freeze (go install github.com/charmbracelet/freeze@latest)
# and python3 with Pillow.
set -euo pipefail

cd "$(dirname "$0")/.."
out=docs/screenshots
work=$(mktemp -d)
sock=zorp-shots-$$
tmux="tmux -L $sock -f $work/tmux.conf"
demo_pid=
cleanup() {
	$tmux kill-server 2>/dev/null || true
	[[ -n $demo_pid ]] && kill "$demo_pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT

freeze=$(command -v freeze || echo "$(go env GOPATH)/bin/freeze")
go build -o "$work/zorp" ./cmd/zorp
go build -o "$work/demo" ./scripts/demo

mkdir -p "$work"/{config/zorp,data,run} "$out"
chmod 700 "$work/run"
cat >"$work/config/zorp/config.toml" <<'EOF'
[auth]
client_id = "00000000-0000-0000-0000-000000000000"

[ui]
color = "tint"

# Tokyo Night's hues; a real terminal reports its own.
[theme]
accent = "#7aa2f7"
header = "#7dcfff"
link = "#7aa2f7"
warning = "#e0af68"
error = "#f7768e"
EOF
# tmux answers zorp's background and foreground queries with these.
cat >"$work/tmux.conf" <<'EOF'
set -g status off
set -g window-style 'bg=#1a1b26,fg=#c0caf5'
set -g window-active-style 'bg=#1a1b26,fg=#c0caf5'
EOF
export XDG_CONFIG_HOME=$work/config XDG_DATA_HOME=$work/data XDG_RUNTIME_DIR=$work/run
"$work/demo" >"$work/demo.log" 2>&1 &
demo_pid=$!
for _ in $(seq 50); do [[ -S $work/run/zorp.sock ]] && break; sleep 0.1; done

# zorp runs as if outside tmux, so it uses 24-bit colour: with no client
# attached, tmux can't vouch for it.
launch() {
	rm -f "$work/data/zorp/ui-state.json"
	$tmux kill-server 2>/dev/null || true
	sleep 0.3
	$tmux new-session -d -x 120 -y "${1:-34}" \
		"env -u TMUX TERM=xterm-256color COLORTERM=truecolor $work/zorp"
	sleep 2
}
keys() {
	for k in "$@"; do
		$tmux send-keys "$k"
		sleep 0.4
	done
	sleep 1
}
shot() {
	$tmux capture-pane -e -p >"$work/$1.ans"
	"$freeze" -l ansi --window -b '#1a1b26' -p 20 -m 0 -r 8 --font.size 14 --line-height 1.25 \
		-o "$work/$1.png" <"$work/$1.ans" >/dev/null
	python3 - "$work/$1.png" "$out/$1.png" <<'EOF'
import sys
from PIL import Image
im = Image.open(sys.argv[1]).convert("RGB")
im = im.resize((im.width // 2, im.height // 2), Image.LANCZOS)
im.quantize(colors=256, method=Image.Quantize.MEDIANCUT, dither=Image.Dither.NONE).save(sys.argv[2], optimize=True)
EOF
	echo "wrote $out/$1.png"
}

launch
keys j
shot inbox

launch 46
keys T Enter
keys Home
shot conversation

launch
keys Space j j Space j Space k k k k
shot select
