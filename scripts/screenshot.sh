#!/usr/bin/env bash
# Retakes .github/screenshot.png, the README's picture: the showcase panel
# (cmd/showcase_test.go) in the light theme on the left, dark on the right.
# Needs Chrome or Chromium and ImageMagick.
set -euo pipefail

addr=127.0.0.1:8091
out=.github/screenshot.png
chrome=$(command -v google-chrome || command -v chromium || command -v chromium-browser) || {
	echo "needs Chrome or Chromium" >&2
	exit 1
}
tmp=$(mktemp -d)
trap 'kill "$server" 2>/dev/null; rm -rf "$tmp"' EXIT

go test -c -o "$tmp/cmd.test" ./cmd
HAKOBU_UI_SHOWCASE=$addr "$tmp/cmd.test" -test.run TestShowcase -test.timeout 0 >"$tmp/server.log" 2>&1 &
server=$!
for _ in $(seq 30); do
	curl -sf -o /dev/null "http://$addr/" && break
	sleep 1
done

shot() {
	"$chrome" --headless=new --disable-gpu --hide-scrollbars --force-device-scale-factor=2 \
		--window-size=1280,900 --virtual-time-budget=3000 "${@:2}" \
		--screenshot="$1" "http://$addr/projects/acme" 2>/dev/null
}
shot "$tmp/light.png"
shot "$tmp/dark.png" --force-dark-mode

magick -size 2560x1800 xc:black -fill white -draw "polygon 0,0 1480,0 1080,1800 0,1800" "$tmp/mask.png"
magick "$tmp/dark.png" "$tmp/light.png" "$tmp/mask.png" -composite \
	-resize 1600x -strip -define png:compression-level=9 "$out"
echo "wrote $out"
