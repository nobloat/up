# Agents

Follow the nobloat skills ([nobloat/skill](https://github.com/nobloat/skill), checked out next to this repo as `../skill`):

- `nobloat`: stack, code, HTTP, config, tests, UI and the plan-first workflow. Always.
- `nobloat-auth`: single-user basic auth, nobloat/auth via caddy forward auth, groups, API keys and service users. For anything about login, roles, access or keys.
- `nobloat-storage`: config, data layout, seeding, append-only logs, backup and export. For anything that writes files.
- `nobloat-system`: integrations between bill, track, tax, up and auth, money and documents, local stack, reviews, demo video. For changes that span services.
- `nobloat-ui`: the shared look (nobloat/ui, `../ui/skill/SKILL.md`). For any page, template, style or front-end behavior; `make ui` copies `ui.css`, `ui.js`, `partials.html` and `i18n/ui/*.json` unchanged, app styles go in `web/static/app.css`, app code in `app.js`.

Agents without skill support: read `../skill/<name>/SKILL.md`; `nobloat-auth` lives in the auth repo: `../auth/skill/SKILL.md`.
