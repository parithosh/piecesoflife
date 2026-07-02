# PiecesOfLife

A self-hosted, private newsletter for a small circle of friends or family.

Every round ("issue"), members answer a handful of questions — text, photos,
links, audio, or camera recordings — plus a free-form **photo & video dump**.
When the round closes, everything is woven into a magazine-style issue the
whole loop can read, react to, and comment on. Then the next round opens by
itself.

No feeds, no ads, no public anything: one small group, one URL, one SQLite
file.

## Features

- **Rounds on a cadence** — monthly, biweekly, or quarterly. Publishing one
  issue pre-creates the next; members can suggest questions for it while
  reading the current one, and it opens for answers automatically on
  schedule (reminder emails included).
- **Rich answers** — text with autosave, photos, links (YouTube/Spotify/…
  embeds), audio and video recorded straight from the browser.
- **The photo & video dump** — a collage closer page per issue for
  everything that didn't fit a question.
- **Magazine reading view** — one question per spread with a pager, or all
  at once; drop caps, comments, reactions-free calm.
- **Media page** — every photo, video, and audio ever shared, grouped by
  issue.
- **Admin loom** — progress ledger, nudges, deadline extension, live
  question editing (with an "already answered" warning), question bank,
  data export (JSON or full ZIP).
- **Email via SMTP or JMAP** — magic-link login, round-opened / reminder /
  published / comment notifications. Works great with a
  [Fastmail API token](docs/fastmail-jmap.md).
- **Single binary** — Go + SQLite + server-rendered templates. ffmpeg
  (bundled in the Docker image) remuxes browser recordings so they seek
  properly.

## Quickstart (development)

Development mode needs no mail account and no secrets: outgoing email is
captured in the logs instead of sent, and `/dev/login` gives you instant
sessions.

```bash
docker compose -f docker-compose.dev.yml up --build
```

Then open <http://localhost:8090> and either:

- visit `http://localhost:8090/dev/login?email=admin@example.com` for an
  instant admin session, or
- request a magic link on the login page and copy it from
  `docker compose -f docker-compose.dev.yml logs`.

The first boot seeds an admin user (`admin@example.com` in dev) and walks
you through a short setup wizard. Templates and static assets hot-reload
from your working tree.

Without Docker (Go 1.25+, ffmpeg optional but recommended):

```bash
PORT=8090 BASE_URL=http://localhost:8090 DEV_MODE=true \
DATABASE_PATH=./data/dev.db UPLOAD_PATH=./data/uploads \
EMAIL_PROVIDER=smtp SMTP_HOST=x SMTP_USER=x SMTP_PASS=x \
FROM_EMAIL=noreply@example.com ADMIN_EMAIL=admin@example.com \
SESSION_SECRET=dev-only go run .
```

## Deployment (production)

```bash
cp .env.example .env   # fill it in — see the comments in the file
docker compose up -d --build
```

The compose file refuses to start without `BASE_URL`, `FROM_EMAIL`,
`ADMIN_EMAIL`, and `SESSION_SECRET` set — there are deliberately no
production defaults for those. Generate the session secret with
`openssl rand -hex 32`.

Run it behind any TLS-terminating reverse proxy (Caddy, nginx, Traefik)
that forwards to the app port. `BASE_URL` must be the public HTTPS URL —
it's baked into every email link.

**Email**: set `EMAIL_PROVIDER=smtp` with classic credentials, or
`EMAIL_PROVIDER=jmap` with a Fastmail API token — the
[Fastmail JMAP guide](docs/fastmail-jmap.md) walks through the whole setup,
ending with the admin **Send test email** button to prove delivery works.

**Backups**: everything lives in the `app-data` volume — one SQLite
database (`/data/db`) and the uploads directory (`/data/uploads`). Snapshot
the volume, or use Admin → Settings → Data export for a ZIP from the UI.

### Configuration reference

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `BASE_URL` | ✳ | — | Public URL used in email links |
| `PORT` | | `8080` | Listen port |
| `DEV_MODE` | | `false` | Dev conveniences; **never in production** |
| `DATABASE_PATH` | ✳ | — | SQLite file path |
| `UPLOAD_PATH` | ✳ | — | Media uploads directory |
| `ADMIN_EMAIL` | ✳ | — | First admin account, seeded on boot |
| `SESSION_SECRET` | ✳ | — | Signs CSRF cookies; long random string |
| `EMAIL_PROVIDER` | | `smtp` | `smtp` or `jmap` |
| `FROM_EMAIL` | ✳ | — | Sender address (must be sendable by your account) |
| `FROM_NAME` | | — | Display name shown in inboxes |
| `SMTP_HOST/PORT/USER/PASS` | smtp | port `465` | Implicit-TLS SMTP submission |
| `JMAP_SESSION_URL` | jmap | Fastmail's | JMAP session endpoint |
| `JMAP_API_TOKEN` | jmap | — | Bearer token ([guide](docs/fastmail-jmap.md)) |
| `LOG_LEVEL` / `LOG_FORMAT` | | `info` / `json` | Logging |

Uploads are capped at 200 MB per file.

## Development notes

```bash
go test ./...                      # unit + integration tests (no network)
cd tests && npm install && npm run snap   # Playwright screenshot crawl of every page
```

The UI is a hand-rolled design system ("Pallu", saree-textile inspired) in
`static/pallu.css`. Database migrations are embedded and run automatically
on boot.
