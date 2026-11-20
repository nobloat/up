# up: plan

What `up` does is in [README.md](README.md). This file keeps decisions and open questions.

## Decisions (owner decision 2026-11-20, review fixes UP-1..UP-14)

- Monitors and webhook/ilert notifiers dial through a guard (`net.Dialer.Control`, on the resolved address, so DNS and redirects are covered): never loopback, unspecified, link-local, or up's own listener address and port. Private networks stay allowed, the stack's monitors check containers. There is no setting to allow loopback.
- Not guarded: SMTP notifiers and `heartbeat.urls` (both only in admin-owned files, a local MTA is common), and anything sent through `proxy` (the proxy decides).
- Monitor and notifier `headers.*` refuse `Remote-*` and `X-Forwarded-*`. A failed `json_match` echoes at most 80 bytes of the value; every reason at most 1000 bytes.
- Basic auth: 10 failures in 10 minutes per client IP (behind a trusted proxy: the last `X-Forwarded-For` entry) answer 429 until the window passes; failures are logged.
- Audit logs: uptime sums and the newest 200 rows are kept in memory, the file is read once at startup. A damaged record is skipped and logged; a torn end of at most 64 KB is cut off at startup; larger damage still stops startup. `-check` reports every damaged record.
- A monitor created in the UI under the name of a deleted one moves the old log aside (`<name>.<time>.deleted.csv`).
- Viewers see a monitor URL as scheme and host only, and no body or header values.
- `/api/export` builds the archive in a `.tmp-export-*` file of the data dir under the store lock and streams it after unlocking.

## Open questions

1. Deleting (or removing by reload) a monitor that is red never resolves its ilert alert and sends no "removed" message. Default: send a resolve/"removed" change to each notifier that was last told red, and write a `reported` row so a monitor that comes back (e.g. after a temporarily broken file) alerts again. Not built yet.
