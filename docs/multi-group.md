# Multi-group ("many circles, one instance") design

Status: **merged to `main`** (PR #1).

> Naming: the UI calls a group a **"circle"** and its admin a
> **"superadmin"**. This design doc predates that rename and uses the
> internal names — **Loop** in prose, `groups` in the schema, `/loops` in
> routes. They are all the same thing.

## Goal

Today one deployment of PiecesOfLife serves exactly one friend group. This change
makes a single deployment able to host **many groups ("Loops")**, each with its
own members, admins, rounds, questions, media, and settings — while keeping the
out-of-the-box experience identical to today: install, run the wizard, and you
have one Loop. Multi-group is opt-in by simply creating a second Loop.

This sets the stage for a shippable product: one hosted instance, many families
and friend circles, one account per person across all of them.

## Vocabulary

| Term | Meaning |
| --- | --- |
| **Instance** | One deployment of the software (one database, one BASE_URL). |
| **Loop** (group) | One friend circle: members, rounds/issues, questions, media, settings. Internally `groups`. |
| **Membership** | A user's relationship to one Loop: role (`admin`/`member`) and active flag. |
| **Instance admin** | The operator. Can create/archive Loops, edit instance settings, and has implicit admin rights in every Loop. |
| **Loop admin** | The superadmin of one Loop (UI term: "circle"). Full admin UI, but only inside that Loop. |

## Identity model

- `users` is **instance-global identity**: one row per email address (name,
  avatar, bio, notification preferences, login). A person signs in once and can
  belong to many Loops.
- `users.role` is **removed**. Per-Loop roles live on `memberships.role`.
- `users.is_instance_admin` marks operators (seeded from `ADMIN_EMAIL`).
- `users.is_active` remains a global kill switch; `memberships.is_active` is
  the per-Loop deactivation used by the members admin page.

## Data model

New tables:

```sql
groups(id, is_active, created_at)                    -- thin; display name lives in settings.loop_name
memberships(id, group_id, user_id, role, is_active, created_at, UNIQUE(group_id, user_id))
instance_settings(id=1, instance_name, allow_public_mementos, created_at, updated_at)
```

Re-scoped tables (migration `016_groups.sql`, `migrate:fk_off` rebuilds):

- `settings`: one row **per group** (`group_id UNIQUE`), same columns as today.
  The existing row becomes group 1's settings.
- `issues.group_id` — everything below an issue (questions, responses, blocks,
  comments, dump items, mementos) inherits its Loop through `issue_id`; guards
  enforce it, queries join through it.
- `question_bank.group_id` — the `used` flag is per-Loop; seed questions are
  topped up per Loop. `UNIQUE(text)` → `UNIQUE(group_id, text)`.
- `default_questions.group_id` — per-Loop default prompts. `UNIQUE(group_id, text)`.
- `sessions.group_id` (nullable) — the session's *current Loop*.
- `users.last_group_id` (nullable) — where a fresh login lands.
- `email_log.group_id` (nullable) — so the admin email log is per-Loop.

Existing single-group data migrates to `group_id = 1` wholesale; a fresh
database gets group 1 created at startup so the current onboarding flow is
untouched.

## Settings: instance level vs group level

- **Group settings** (`settings`, one row per Loop): loop name, tagline,
  frequency, submission window, timezone, invite note, accent color,
  auto-create, questions per issue, public mementos — everything the admin
  settings page shows today. Each Loop's admin owns them.
- **Instance settings** (`instance_settings`): `instance_name` (login page and
  operator console branding) and `allow_public_mementos` as an **instance
  policy**: a Loop can only expose public mementos if *both* the instance
  policy and the Loop's own setting allow it (logical AND). This is the
  override pattern for future policy-style settings: instance sets the
  ceiling, groups opt in underneath it.
- New Loops are created with the same defaults a fresh install gets; their
  admin then runs the same setup wizard the first Loop used.

## Request context & authorization

Session stays global (one cookie, one login). Each request resolves:

1. **User** — from the session cookie (unchanged).
2. **Current Loop** — `sessions.group_id` if it still maps to an active
   membership in an active group; otherwise fall back to `users.last_group_id`,
   then the user's only/first active membership. Instance admins without any
   membership fall back to the instance console.
3. **Membership** — the (user, current group) row; `membership.role` drives
   `adminMiddleware` (instance admins pass implicitly).

Context carries `user`, `group` (with its settings' loop name), and
`membership`. Handlers never trust a client-supplied group id for scoping —
the current Loop comes from the session, and every resource handler verifies
the resource belongs to it.

**Resource-based auto-switch:** URLs stay exactly as they are today
(`/issues/2026/7`, `/issues/12/respond`, …). If a signed-in user opens a link
to a resource in *another* Loop they belong to, the request transparently
switches their current Loop to that resource's Loop (session + last_group_id
updated) instead of 404ing. This keeps every email link, bookmark, and shared
URL working without group-prefixed routes, and matches how people actually
move between Loops (via links, not menus).

Explicit switching: `POST /api/me/group {group_id}` from the nav switcher.

## UX

Guiding rule: **a person in one Loop never sees multi-Loop chrome.** All of the
below appears only when it applies.

- **Nav Loop switcher** (base.html): shown only when the user has ≥2 active
  memberships. The current Loop's name is the label; the menu lists the other
  Loops plus "All your Loops". Switching reloads the equivalent page in the
  new Loop.
- **/loops — "Your Loops"** page: one card per membership (Loop name, tagline,
  your role, round status: collecting/deadline, latest edition). Landing sends
  multi-Loop users here when there is no obvious current Loop; single-Loop
  users never see it.
- **Instance console — /instance** (instance admins only): list of Loops
  (name, members, current round status), "Start a new circle of friends"
  (name + superadmin's email → creates the Loop, invites its admin), archive
  Loop, and instance
  settings (instance name, public-memento policy). Linked from the nav for
  instance admins only. Styled with the same Pallu system as the admin pages.
- **New-Loop bootstrap:** creating a Loop invites its superadmin by email.
  When that superadmin enters the new Loop (login or switcher), they get the existing
  setup wizard (`/admin/setup`) scoped to that Loop; members who arrive before
  setup completes see a friendly "this Loop is still being set up" note
  instead of broken pages.
- **Members page** shows per-Loop roles/deactivation (memberships), and
  inviting an email address that already has an account simply adds that
  person to the Loop (their profile, name, and login carry over).
- **Login is unchanged** (email magic link, instance-wide). After login:
  last-used Loop → its issues page; several Loops and no history → /loops.
- **Emails** carry the Loop's name (invites, reminders, published, comment
  notifications); the login email uses the instance name since it is
  Loop-agnostic.

## Scheduler

Issue-bound events (`reminder_*`, `auto_close`, `create_next_issue`) resolve
their Loop through `issues.group_id` and load that Loop's settings for
timezone/frequency/copy (migration 017 backfills the pre-multi-group events
that carried no issue reference). Global cleanup events are untouched.
Auto-create works independently per Loop.

Archiving a Loop cancels its pending events, and the scheduler additionally
skips any event whose Loop has since been archived — an archived Loop never
closes rounds or emails members. A daily reconcile sweep re-queues any
auto-create cycle that stalled (e.g. a transient store error at publish time
meant no next-round event was queued).

## Backwards compatibility / rollout

- Migration 016 moves existing data to group 1 with no behavior change;
  existing admins become admins of Loop 1. Only the `ADMIN_EMAIL` account is
  flagged instance admin (by startup seeding, not the migration) — co-admins
  of the original Loop do not gain authority over Loops created later.
- No URL changes; no email re-linking; sessions survive (group_id backfills
  lazily on next request).
- The beta instance on `main` is unaffected until this branch merges; the
  migration is designed to be rehearsed against a copy of the live DB like
  013–015 were.

## Explicitly out of scope (future work)

- Self-serve Loop signup (Loops are operator-created for now).
- Per-Loop custom domains/paths, per-Loop email sender identities.
- Cross-Loop anything (shared albums, cross-posting).
- Per-membership notification preferences (they stay per-user).
- `/uploads/` file serving stays authenticated-instance-wide: file URLs are
  unguessable random names, but a member of Loop A who somehow obtains a
  Loop B file URL could fetch it. Per-file group checks would need a
  path→owner index; noted for the productization pass.
