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

# A browser window around each, the dark one cut on a diagonal over the
# light one, on a transparent background.
cat >"$tmp/frame.html" <<'HTML'
<!doctype html>
<meta charset="utf-8">
<style>
  html, body { margin: 0; background: transparent; }
  body { padding: 24px 40px 48px; font: 13px -apple-system, "Segoe UI", system-ui, sans-serif; }
  .stack { position: relative; width: 1280px; }
  .win { border-radius: 12px; overflow: hidden; background: #f6f8fa; outline: 1px solid rgb(0 0 0 / .12);
    box-shadow: 0 24px 48px -12px rgb(0 0 0 / .35), 0 8px 16px -8px rgb(0 0 0 / .2); }
  .win img { display: block; width: 1280px; height: 900px; }
  .bar { height: 44px; display: flex; align-items: center; padding: 0 16px; background: #e8e9ec; border-bottom: 1px solid #d5d7db; }
  .dots { display: flex; gap: 8px; width: 120px; }
  .dots i { width: 12px; height: 12px; border-radius: 50%; }
  .dots i:nth-child(1) { background: #ff5f57; } .dots i:nth-child(2) { background: #febc2e; } .dots i:nth-child(3) { background: #28c840; }
  .url { flex: 0 1 480px; margin: 0 auto; transform: translateX(-60px); height: 28px; border-radius: 8px; display: flex;
    align-items: center; justify-content: center; background: #fff; color: #57606a; }
  .dark { position: absolute; inset: 0; clip-path: polygon(57.8% 0, 100% 0, 100% 100%, 42.2% 100%); }
  .dark .win { background: #0d1117; outline-color: rgb(255 255 255 / .2); }
  .dark .bar { background: #24272d; border-color: #33363c; }
  .dark .url { background: #15171b; color: #9198a1; }
</style>
<div class="stack">
  <div class="win"><div class="bar"><span class="dots"><i></i><i></i><i></i></span><span class="url">hakobu.example.com/projects/acme</span></div><img src="light.png"></div>
  <div class="dark"><div class="win"><div class="bar"><span class="dots"><i></i><i></i><i></i></span><span class="url">hakobu.example.com/projects/acme</span></div><img src="dark.png"></div></div>
</div>
HTML
"$chrome" --headless=new --disable-gpu --hide-scrollbars --force-device-scale-factor=2 \
	--window-size=1360,1016 --default-background-color=00000000 --allow-file-access-from-files \
	--screenshot="$tmp/framed.png" "file://$tmp/frame.html" 2>/dev/null
magick "$tmp/framed.png" -resize 1600x -strip -define png:compression-level=9 "$out"
echo "wrote $out"
