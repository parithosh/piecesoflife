# Sending email through Fastmail (JMAP)

PiecesOfLife can deliver all of its email — magic-link logins, round-opened
announcements, reminders, publish notifications, comment notifications —
through Fastmail's JMAP API instead of SMTP. You mint one API token, put it
in `.env`, and you're done: no SMTP passwords, no app-specific passwords,
and the token can be revoked independently of your account password.

Under the hood the app resolves your JMAP session once, creates each email
as a draft, submits it, and files it into your Sent folder — so everything
the circle sends is visible in your own Sent mail. If Fastmail ever
invalidates the session (say you rotate the token), the app re-bootstraps
and retries automatically.

## 1. Decide which address the circle sends as

`FROM_EMAIL` must be an address your Fastmail account is allowed to send
as — your main address or any **sending identity/alias** you've configured
(Fastmail Settings → **Mail** → **Sending identities**).

A dedicated alias like `letters@yourdomain.com` is nice: replies and
bounces stay separated from your personal mail. Create the alias first and
send one test mail from the Fastmail web UI to confirm it works.

> If `FROM_EMAIL` doesn't match any identity, the app logs a warning and
> falls back to your first identity — mail still goes out, but from the
> wrong address. Match it exactly.

Set `FROM_NAME` to whatever should appear next to the address in inboxes —
usually your circle's name (e.g. `chaos crew`).

## 2. Mint the API token

1. Log in to Fastmail and open **Settings → Privacy & Security**.
2. Find **Connected apps & API tokens** and choose **Manage API tokens →
   New API token**. (Fastmail may ask for your password again.)
3. Name it something recognizable, e.g. `piecesoflife`.
4. Scopes: the token needs **Mail (read/write)** and **Email Submission**.
   - Read/write mail is required because the app creates the outgoing
     message as a draft and moves it to Sent after submission.
   - If Fastmail's UI offers a "send-only"/submission-only token, that is
     *not* enough on its own.
5. Leave "Read-only" unchecked, create the token, and **copy it
   immediately** — Fastmail shows it only once. It looks like
   `fmu1-xxxxxxxx-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx`.

Treat the token like a password. If it ever leaks, revoke it on the same
settings page — the app will start failing sends with `401`, and a new
token in `.env` plus a container restart fixes it.

## 3. Configure the app

In your `.env` (see `.env.example`):

```dotenv
EMAIL_PROVIDER=jmap
JMAP_SESSION_URL=https://api.fastmail.com/jmap/session
JMAP_API_TOKEN=fmu1-…your token…
FROM_EMAIL=letters@yourdomain.com
FROM_NAME=chaos crew
```

Then restart:

```bash
docker compose up -d
```

On the first send you should see `JMAP session initialized` in the logs
(`docker compose logs -f app`), with the resolved account and identity IDs.

## 4. Prove it end to end

Log in as the admin and open **Admin → Settings → Email delivery**. It
shows the active provider and sender; press **Send test email**. Within a
few seconds you should have a test message in the `ADMIN_EMAIL` inbox —
sent via JMAP, and visible in your Fastmail Sent folder too.

If the button reports an error, the message is the raw provider error —
see troubleshooting below.

## 5. Deliverability (worth 5 minutes)

Emails come from your own Fastmail account, so deliverability is
Fastmail's usual story:

- If `FROM_EMAIL` is on your **own domain**, make sure the domain is set up
  in Fastmail (Settings → Domains) — Fastmail gives you the exact MX,
  SPF, and DKIM records. Without DKIM, Gmail and Outlook will happily junk
  your circle's reminders.
- If it's an `@fastmail.com` address, there's nothing to do.

## Troubleshooting

| Symptom | Likely cause / fix |
|---|---|
| `session endpoint returned 401` / `JMAP endpoint returned 401` | Token wrong, revoked, or pasted with whitespace. Mint a new one. |
| `forbiddenFrom` when sending | `FROM_EMAIL` isn't a sending identity on the account. Add the identity in Fastmail or fix the address. |
| Warning: `No JMAP identity matches FROM_EMAIL` | Same as above — mail goes out from your first identity until fixed. |
| `account is missing a drafts or sent mailbox` | Extremely unusual — the token's account has no standard mailboxes; check you minted the token on the right account. |
| Test email arrives but real notifications don't | Check `docker compose logs` for per-recipient errors; each send is logged with the recipient and error. |
| Everything worked, then 401s after months | You rotated/revoked the token. Update `.env`, restart. The app retries each send once with a fresh session automatically. |

## Notes

- `JMAP_SESSION_URL` only changes if you're pointing at a non-Fastmail
  JMAP provider; any server implementing `urn:ietf:params:jmap:mail` +
  `submission` should work.
- Sends are throttled slightly (100 ms apart) to stay friendly with rate
  limits; a circle of a dozen members is nowhere near Fastmail's caps.
