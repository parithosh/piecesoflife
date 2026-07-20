# Getting started: from empty server to first issue

This is the operator's walkthrough. At the end of it you'll have a running
instance, a set-up circle with invited members, and your first round
collecting answers. Budget about fifteen minutes, most of which is writing
the invite note.

## What you need

- A server with Docker and a reverse proxy that terminates TLS (Caddy,
  nginx, Traefik — anything that can forward to a port).
- A domain (or subdomain) pointed at it. Its public HTTPS URL becomes
  `BASE_URL`, which is baked into every email link — decide it before the
  first boot.
- An email account the app can send from: any SMTP provider, or Fastmail
  via a JMAP API token ([guide](fastmail-jmap.md)).

## 1. Deploy

Copy the environment template and fill it in — every value is explained by
the comments in the file:

```bash
cp .env.example .env
```

Then start the app, either from the prebuilt image:

```bash
mkdir -p db uploads && chown -R 100:101 db uploads
docker compose -f docker-compose.deploy.yml up -d
```

or building from source:

```bash
docker compose up -d --build
```

The compose files fail loudly if `BASE_URL`, `FROM_EMAIL`, `ADMIN_EMAIL`,
or `SESSION_SECRET` are missing — that's intentional. Check it's alive:

```bash
curl -fsS localhost:8080/health
```

Point your reverse proxy at the app port, and open your `BASE_URL`.

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
| `JMAP_API_TOKEN` | jmap | — | Bearer token ([guide](fastmail-jmap.md)) |
| `LOG_LEVEL` / `LOG_FORMAT` | | `info` / `json` | Logging |

Uploads are capped at 200 MB per file.

## 2. First login

The account from `ADMIN_EMAIL` is created on first boot. Enter that email
on the login page and you'll get a magic link — there are no passwords
anywhere in the app.

> If the login email never arrives, your email settings need attention —
> see [verifying delivery](#5-verify-email-delivery) below. The container
> logs (`docker compose logs -f app`) show every send attempt and error.

## 3. The setup wizard

The first visit as admin lands on a four-step wizard:

1. **About you** — the name members will see on emails from you.
2. **Your newsletter** — the circle's name and tagline, the cadence
   (monthly, biweekly, or quarterly), how many days members get to answer,
   your timezone, and when the first round opens.
3. **Pick questions** — every issue starts with your default questions;
   untick any you don't want, add picks for the first issue, or write your
   own. The ★ on a pick makes it a default in every future issue too.
4. **Invite friends** — email addresses plus an optional personal note.
   You can invite more people at any time from Admin → Members.

Press **Launch** and the first round is live: invites go out, the deadline
is scheduled, and reminder emails are queued automatically.

Everything chosen in the wizard can be changed later under Admin →
Settings — including the default questions, which get a full manager with
reordering and per-question on/off switches.

## 4. Running a round

The admin dashboard (**Admin** in the nav) is the loom for the current
round:

- **Progress** — who has submitted, who's still writing, one-click
  **Nudge** buttons for the rest. Reminders also go out automatically at
  three days and one day before the deadline, to non-responders only.
- **Count me in this round** — whether you, the admin, count toward the
  progress bar.
- **Questions in this round** — edit, reorder, add, or remove questions
  *live*, even mid-round. Editing a question someone already answered gets
  a warning first.
- **Adjust schedule** — move the round's close date (either direction) and
  pin exactly when the next round opens and closes, e.g. "opens the 15th,
  closes the 25th". Reminders are requeued around the new dates, and future
  rounds keep the new rhythm.
- **Publish now** — closes the round early and publishes whatever's in.

Left alone, the round closes itself at the deadline and publishes
automatically. Either way every member gets an email whose link signs them
straight into the reading view.

Admin → **Members** handles the roster: invite more people (inviting an
address that already has an account on the instance simply adds that
person to your circle), or deactivate someone who's left.

## 5. Verify email delivery

Before the first deadline, prove the mail path end to end: open Admin →
Settings → **Email delivery** and press **Send test email**.

A test message should land in the `ADMIN_EMAIL` inbox within seconds. If
the button reports an error, it shows the raw provider error — the
[Fastmail guide's troubleshooting table](fastmail-jmap.md#troubleshooting)
covers the common ones.

## 6. After the first issue

- **The next round creates itself.** Publishing pre-creates it on your
  cadence; members can suggest questions for it from the bottom of the
  published issue, and it opens for answers on schedule. The dashboard's
  "Start it now" opens it early.
- **The archive grows** under Issues, and every photo/video/audio ever
  shared is browsable under Media.
- **Data export** — Admin → Settings offers the full issue history as JSON
  or a ZIP including all uploads.
- **Backups** — everything lives in the data directory (SQLite database +
  uploads). Snapshot it however you back up the host. Upgrades snapshot
  the database automatically before migrating; see
  [upgrading.md](upgrading.md).

## 7. Growing beyond one circle

One instance can host several circles — separate members, rounds, and
settings, one login per person. As instance admin you get a **Console**
page for starting a new circle (you name it and invite its superadmin, who
then runs the same wizard you just did), archiving old ones, and
instance-wide settings.

The design and internals are documented in [multi-group.md](multi-group.md).
