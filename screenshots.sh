#!/usr/bin/env bash
# Screenshots of the UI and a status page: desktop/phone x light/dark, plus screenshots/overview.png
# (needs Chrome or Chromium, ImageMagick for the overview). Works with bash 3.2 (macOS).
set -euo pipefail
cd "$(dirname "$0")"

chrome=$(command -v chromium || command -v chromium-browser || command -v google-chrome || true)
mac="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
[ -n "$chrome" ] || { [ -x "$mac" ] && chrome=$mac; } || { echo "screenshots: chromium or google-chrome not found" >&2; exit 1; }

port=7496 out=screenshots tmp=$(mktemp -d)
base="http://localhost:$port" self="http://127.0.0.1:$port"
trap 'jobs -p | xargs kill 2>/dev/null || true; rm -rf "$tmp"' EXIT
! curl -s -o /dev/null "$base" || { echo "screenshots: port $port is in use" >&2; exit 1; }
rm -rf "$out"/*/ && mkdir -p "$out/pages"
CGO_ENABLED=0 go build -o "$tmp/up" .

# monitors check up itself: static files are up, missing paths are down
cfg=$tmp/config
mkdir -p "$cfg"
conf() { cat >"$cfg/$1.conf"; } # conf <file name without .conf>, content on stdin
conf up <<CONF
listen = 127.0.0.1:$port
base_url = $base
data_dir = $tmp/data
auth.user = admin
auth.password = admin
CONF
http() { # http <name> <tags> <path> <description> [extra line]
	printf 'type = http\ntags = %s\ninterval = 1h\nurl = %s\ndescription = %s\n%s\n' "$2" "$self$3" "$4" "${5:-}" | conf "$1.monitor"
}
http website "public, prod" /static/style.css "Home page answers with 2xx"
http api "public, prod" /static/favicon.svg "Public API health endpoint"
http shop "public, prod" /checkout "Checkout page answers with 2xx" "retry_count = 3"
http docs public /static/style.css "Documentation site"
http legacy-admin internal /legacy "Old admin backend, scheduled for removal"
printf 'type = push\ntags = backup\ninterval = 24h\ntoken = screenshot-token-0001\ndescription = Nightly file backup reports in every 24h\n' | conf nightly-backup.monitor
printf 'type = push\ntags = backup\ninterval = 24h\nretry_count = 2\ntoken = screenshot-token-0002\ndescription = Database dump reports in every 24h\n' | conf db-backup.monitor
printf 'title = Example Status\ntag = public\n' | conf public.page
"$tmp/up" -config "$cfg" >"$tmp/log" 2>&1 &
pid=$!
until curl -s -o /dev/null "$base/static/style.css"; do
	kill -0 "$pid" 2>/dev/null || { echo "screenshots: server died:" >&2; cat "$tmp/log" >&2; exit 1; }
	sleep .1
done
for m in website api shop docs legacy-admin; do curl -s -o /dev/null -u admin:admin -X POST "$base/api/monitors/$m/check"; done
curl -s -o /dev/null -X POST --data '{"files": 1284, "bytes": "38G"}' "$base/push/nightly-backup/screenshot-token-0001"
curl -s -o /dev/null "$base/push/db-backup/screenshot-token-0002" # green first, so the failure below is yellow
curl -s -o /dev/null "$base/push/db-backup/screenshot-token-0002?status=down"
curl -s -o /dev/null -u admin:admin -H 'Content-Type: application/json' --data '{"reason": "maintenance until 14:00"}' "$base/api/monitors/docs/pause"
sleep 1

# reduced motion: headless Chrome freezes CSS animations on their first frame
# phone is 500px wide: headless Chrome never lays out narrower, a smaller window only crops a 500px page
for page in list=/ red=/?state=red search=/?q=backup monitor=/monitor/shop paused=/monitor/docs status=/status/public; do
	for size in desktop=1280,900 phone=500,900; do
		for scheme in light=1 dark=0; do
			"$chrome" --headless=new --disable-gpu --no-sandbox --hide-scrollbars --run-all-compositor-stages-before-draw --force-prefers-reduced-motion \
				--user-data-dir="$tmp/chrome" --window-size="${size#*=}" --blink-settings="preferredColorScheme=${scheme#*=}" \
				--timeout=3000 --screenshot="$out/pages/${page%%=*}-${size%%=*}-${scheme%%=*}.png" \
				"http://admin:admin@localhost:$port${page#*=}" >/dev/null 2>&1
		done
	done
done
if command -v magick >/dev/null; then
	p="$out/pages/list"
	magick "$p-desktop-light.png" "$p-desktop-dark.png" "$p-phone-light.png" "$p-phone-dark.png" \
		-resize x600 -background none -splice 16x0 +append -chop 16x0 -strip "$out/overview.png"
fi
echo "screenshots: $(find "$out" -name '*.png' | wc -l | tr -d ' ') images in $out/"
