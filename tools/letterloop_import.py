#!/usr/bin/env python3
"""One-shot importer: Letterloop issue PDFs -> PiecesOfLife.

Deliberately external to the Go binary: this runs once per migrating Loop
and then never again, so it lives here instead of adding an import path,
admin UI, and a permanent code surface to the app.

Two stages, with a reviewable JSON bundle in between:

  1. extract  PDFs            -> bundle.json + media/ + people.json
  2. load     bundle.json     -> SQLite rows + files under UPLOAD_PATH

Stage 1 never touches the app; stage 2 never re-reads a PDF. Between them
you fix names, emails, prompts, and anything the layout heuristics got
wrong, by editing plain JSON.

Usage:

    pip install pymupdf
    python3 tools/letterloop_import.py extract \
        --pdf-dir letterloop-export --out _local/letterloop
    # fill in the emails in _local/letterloop/people.json
    python3 tools/letterloop_import.py load \
        --bundle _local/letterloop \
        --db /data/db/piecesoflife.db \
        --upload-path /data/uploads \
        --group 1

Stop the app before `load`, and back up the database first. The whole load
is one transaction: it either lands completely or not at all.

What the PDFs do NOT contain, and therefore cannot be imported: comment
bodies (only "3 comments" counts are printed), audio files, and video
files (a video survives as its poster frame plus a duration).
"""

from __future__ import annotations

import argparse
import json
import re
import shutil
import sqlite3
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path

# Only `extract` reads PDFs. `load` is stdlib-only on purpose, so it can run
# on the server (or in a bare python container) with nothing to install.
try:
    import fitz  # PyMuPDF
except ImportError:  # pragma: no cover - operator-facing
    fitz = None

# ---------------------------------------------------------------------------
# Layout constants, measured from the Letterloop export (see README below).
#
# Every issue is one column of blocks in a fixed two-typeface system:
#   SourceSerifPro-Bold 13.5pt  section heading ("✨ Questions")
#   DMSans 12pt at x0 = 145.0   question ("Alex asked: ..." or bare prompt)
#   DMSans 12pt at x0 = 145.9   answer ("Alex: ...") or its continuation
#   DMSans 10.5pt "0:16"        duration badge over a video poster frame
# The 0.9pt indent difference is what separates a question from an answer;
# it held for all 700 blocks across the 35-issue corpus.
# ---------------------------------------------------------------------------

QUESTION_X0 = 145.4  # blocks at or left of this are questions
QUESTION_GAP = 45.0  # vertical gap that always precedes a question block
BODY_SIZE = 12.0
HEADING_SIZE = (13.0, 14.0)
FULL_BLEED_X0 = 140.0  # photo-wall images bleed wider than inline ones
RIGHT_MARGIN = 468.7  # text column's right edge, for paragraph detection

SPEAKER_RE = re.compile(r"^\s*([^:]{1,32}?)\s*:\s*")
ASKED_RE = re.compile(r"^\s*(.{1,32}?)\s+asked:\s*")
DURATION_RE = re.compile(r"^\d+:\d\d$")
COMMENT_RE = re.compile(r"^\d+ comments?$")
URL_RE = re.compile(r"https?://\S+")
ORDINAL_RE = re.compile(r"(\d+)(?:st|nd|rd|th)")

# Letterloop's fixed sections become one question per issue. "Questions" is
# the only section carrying real per-issue prompts; "Photo Wall" is not a
# question at all — it maps onto the issue's photo & video dump.
SECTIONS = {
    "Announcements": {"kind": "prompt", "prompt": "Announcements", "source": "admin"},
    "Questions": {"kind": "questions"},
    "One Good Thing": {"kind": "prompt", "prompt": "One good thing", "source": "default"},
    "Check it Out": {"kind": "prompt", "prompt": "Check it out", "source": "default"},
    "Phone a Friend": {"kind": "prompt", "prompt": "Phone a friend", "source": "default"},
    "On Your Mind": {"kind": "prompt", "prompt": "What's been on your mind?", "source": "default"},
    "Photo Wall": {"kind": "dump"},
}

VIDEO_STILL_CAPTION = "Video still ({duration}) — the video itself is not in the Letterloop export"


# ---------------------------------------------------------------------------
# Stage 1: PDF -> bundle
# ---------------------------------------------------------------------------


def section_key(heading: str) -> str:
    """Strip the leading emoji from a section heading."""
    return heading.split(" ", 1)[-1].strip() if " " in heading else heading.strip()


def flow_blocks(page):
    """Blocks in reading order (single column, so top-to-bottom is enough)."""
    return sorted(
        page.get_text("rawdict")["blocks"],
        key=lambda b: (round(b["bbox"][1], 1), b["bbox"][0]),
    )


def block_lines(block):
    out = []
    for line in block["lines"]:
        chars = [c for span in line["spans"] for c in span["chars"]]
        text = "".join(c["c"] for c in chars)
        if text.strip():
            out.append((text, line["bbox"], chars))
    return out


def paragraphs(lines):
    """Split wrapped lines back into paragraphs.

    Letterloop encodes paragraph breaks purely by ending a line early — the
    leading is identical everywhere — so a break is a line that stopped even
    though the next line's first word would still have fitted.
    """
    paras, current = [], []
    for i, (text, bbox, chars) in enumerate(lines):
        current.append(text.strip())
        if i + 1 == len(lines):
            break
        width = 0.0
        for char in lines[i + 1][2]:  # exact width of the next line's first word
            if char["c"] == " ":
                break
            width += char["bbox"][2] - char["bbox"][0]
        space = next((c["bbox"][2] - c["bbox"][0] for c in chars if c["c"] == " "), 3.3)
        if bbox[2] + space + width < RIGHT_MARGIN - 1:
            paras.append(" ".join(current))
            current = []
    if current:
        paras.append(" ".join(current))
    return [p.strip() for p in paras if p.strip()]


def repair_urls(text: str, uris: list[str]) -> str:
    """Undo line-break damage inside URLs using the PDF's link annotations.

    A wrapped URL extracts as "…/china-builds- world-first-…"; the annotation
    carries the true target, so rewrite the text span to match it.
    """
    for uri in uris:
        if uri in text:
            continue
        loose = re.compile(r"\s*".join(re.escape(c) for c in uri))
        text = loose.sub(uri.replace("\\", "\\\\"), text, count=1)
    return text


def spans_of(block):
    return [s for line in block["lines"] for s in line["spans"]]


def parse_pdf(path: Path) -> dict:
    doc = fitz.open(path)
    head = doc[0].get_text().strip().split("\n")
    match = re.search(r"Issue No\.(\d+)\s*·\s*(.+)", doc[0].get_text())
    if not match:
        raise ValueError(f"{path}: no 'Issue No.N · date' masthead found")

    published = parse_date(match.group(2))
    issue = {
        "number": int(match.group(1)),
        "date": published.strftime("%Y-%m-%d"),
        # The reading view addresses an issue as /issues/{year}/{month}, so
        # these are the issue's identity, not just its label. Editable here
        # because a four-weekly cadence eventually puts two issues in one
        # month; see the collision check in `load`.
        "year": published.year,
        "month": published.month,
        "title": None,
        "loop_name": head[1].strip(),
        "sections": [],
        "lost": [],  # things the PDF proves existed but cannot carry
    }

    section = None  # current section dict
    holder = None  # current answer dict, for continuations and images
    pending_media: list[dict] = []  # full-bleed images awaiting their caption
    last_media = None

    for page in doc:
        links = [(fitz.Rect(l["from"]), l["uri"]) for l in page.get_links() if l.get("uri")]
        prev_bottom = None  # bottom of the previous body block, for the gap test

        for block in flow_blocks(page):
            bbox = block["bbox"]

            if block["type"] == 1:
                media = {
                    "type": "photo",
                    "ext": block["ext"],
                    "width": block["width"],
                    "height": block["height"],
                    "_bytes": block["image"],
                }
                last_media = media
                prev_bottom = bbox[3]
                if bbox[0] < FULL_BLEED_X0 or holder is None:
                    pending_media.append(media)
                else:
                    holder["blocks"].append(media)
                continue

            spans = spans_of(block)
            if not spans:
                continue
            lead = spans[0]
            flat = "".join(c["c"] for s in spans for c in s["chars"]).strip()

            if any(
                s["font"].startswith("SourceSerif")
                and HEADING_SIZE[0] <= s["size"] <= HEADING_SIZE[1]
                for s in spans
            ):
                key = section_key(flat)
                if key not in SECTIONS:
                    raise ValueError(f"{path}: unknown section heading {flat!r}")
                section = {"section": key, "label": flat, **SECTIONS[key], "items": []}
                issue["sections"].append(section)
                holder = None
                continue

            if DURATION_RE.match(flat) and last_media is not None:
                # A duration badge sits on top of a video's poster frame.
                last_media["type"] = "video_still"
                last_media["duration"] = flat
                issue["lost"].append(f"video ({flat})")
                continue

            if COMMENT_RE.match(flat):
                issue["lost"].append(f"{flat} (bodies are not in the export)")
                continue

            if lead["size"] != BODY_SIZE or not lead["font"].startswith("DMSans"):
                continue  # masthead, footer, badges

            paras = paragraphs(block_lines(block))
            if not paras:
                continue
            uris = [u for r, u in links if fitz.Rect(bbox).intersects(r)]
            paras = [repair_urls(p, uris) for p in paras]

            # A question is set one point further left than an answer. One
            # prompt in the corpus lost that indent, so an unbolded block
            # preceded by the section's wide inter-question gap (52.6pt;
            # continuations never exceed 38.4pt) also counts.
            bold = lead["font"].endswith("Bold")
            wide_gap = prev_bottom is not None and bbox[1] - prev_bottom >= QUESTION_GAP
            prev_bottom = bbox[3]

            if bbox[0] <= QUESTION_X0 or (wide_gap and not bold):
                whole = "\n".join(paras)
                asked = ASKED_RE.match(whole)
                text = (whole[asked.end():] if asked else whole).replace("\n", " ")
                section["items"].append(
                    {
                        "question": text.strip().strip("*").strip(),
                        "asked_by": asked.group(1) if asked else None,
                        "answers": [],
                    }
                )
                holder = None
                continue

            speaker = SPEAKER_RE.match(paras[0])
            if bold and speaker:
                body = [paras[0][speaker.end():]] + paras[1:]
                holder = {
                    "speaker": speaker.group(1),
                    "blocks": [t for p in body for t in text_blocks(p)],
                }
                if pending_media:
                    holder["blocks"] = pending_media + holder["blocks"]
                    pending_media = []
                attach_answer(section, holder)
            elif holder is None:
                raise ValueError(f"{path}: text with no speaker: {paras[0][:60]!r}")
            else:
                holder["blocks"].extend(t for p in paras for t in text_blocks(p))

    if pending_media:
        raise ValueError(f"{path}: {len(pending_media)} image(s) with no owner")

    return issue


def text_blocks(paragraph: str) -> list[dict]:
    """A paragraph becomes a text block, or a link block plus its comment.

    PiecesOfLife renders each text block as its own <p> and gives link blocks
    a real embed, so a paragraph that opens with a URL is worth splitting.
    """
    match = URL_RE.match(paragraph)
    if not match:
        return [{"type": "text", "content": paragraph}]
    url = match.group(0).rstrip(".,;:)")
    rest = paragraph[len(url):].strip(" -–—:")
    out = [{"type": "link", "url": url}]
    if rest:
        out.append({"type": "text", "content": rest})
    return out


def attach_answer(section: dict, answer: dict) -> None:
    """File an answer under the section's current question."""
    if section["kind"] == "questions":
        if not section["items"]:
            raise ValueError("answer before any question in the Questions section")
        section["items"][-1]["answers"].append(answer)
        return
    if not section["items"]:
        section["items"].append(
            {
                "question": section.get("prompt", section["section"]),
                "asked_by": None,
                "answers": [],
            }
        )
    section["items"][0]["answers"].append(answer)


def parse_date(raw: str) -> datetime:
    return datetime.strptime(ORDINAL_RE.sub(r"\1", raw).strip(), "%B %d, %Y")


def cmd_extract(args) -> int:
    if fitz is None:
        sys.exit("extract needs PyMuPDF: pip install pymupdf")
    pdfs = sorted(
        Path(args.pdf_dir).glob("*.pdf"),
        key=lambda p: int(re.search(r"(\d+)", p.stem).group(1)),
    )
    if not pdfs:
        sys.exit(f"no PDFs in {args.pdf_dir}")

    out = Path(args.out)
    media_dir = out / "media"
    media_dir.mkdir(parents=True, exist_ok=True)

    issues, people, lost, media_total = [], {}, [], 0
    for pdf in pdfs:
        issue = parse_pdf(pdf)
        issue["source_file"] = pdf.name
        n = 0
        for section in issue["sections"]:
            for item in section["items"]:
                if item["asked_by"]:
                    people.setdefault(item["asked_by"], 0)
                for answer in item["answers"]:
                    people[answer["speaker"]] = people.get(answer["speaker"], 0) + 1
                    for block in answer["blocks"]:
                        if "_bytes" not in block:
                            continue
                        name = f"issue-{issue['number']:02d}-{n:02d}.{block['ext']}"
                        (media_dir / name).write_bytes(block.pop("_bytes"))
                        block["file"] = f"media/{name}"
                        n += 1
                        media_total += 1
        lost += [f"issue {issue['number']}: {x}" for x in issue.pop("lost")]
        issues.append(issue)

    bundle = {
        "source": str(Path(args.pdf_dir)),
        "extracted_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "loop_name": issues[0]["loop_name"],
        "issues": issues,
        "not_imported": sorted(set(lost)),
    }
    (out / "bundle.json").write_text(json.dumps(bundle, indent=2, ensure_ascii=False))

    people_path = out / "people.json"
    if not people_path.exists():
        template = {
            name: {"email": "", "role": "admin" if i == 0 else "member"}
            for i, name in enumerate(sorted(people, key=lambda k: -people[k]))
        }
        people_path.write_text(json.dumps(template, indent=2, ensure_ascii=False))

    answers = sum(
        len(i["answers"]) for iss in issues for s in iss["sections"] for i in s["items"]
    )
    print(f"{len(issues)} issues, {answers} answers, {media_total} media files")
    print(f"people seen: {', '.join(sorted(people))}")
    print(f"wrote {out/'bundle.json'}, {media_dir}/, {people_path}")
    print(f"not importable ({len(lost)}) — see bundle.json 'not_imported':")
    for line in sorted(set(lost)):
        print(f"  {line}")
    if any(not v["email"] for v in json.loads(people_path.read_text()).values()):
        print(f"\nNEXT: fill in the email addresses in {people_path}, then run `load`.")
    return 0


# ---------------------------------------------------------------------------
# Stage 2: bundle -> SQLite + uploads
# ---------------------------------------------------------------------------


def cmd_load(args) -> int:
    bundle_dir = Path(args.bundle)
    bundle = json.loads((bundle_dir / "bundle.json").read_text())
    people = json.loads((bundle_dir / "people.json").read_text())

    # An explicit null email means "not a real member" (e.g. Letterloop's
    # "Anonymous" asker): their questions land unattributed instead of
    # conjuring a user. An empty string is an unfilled template slot.
    missing = [n for n, v in people.items() if v.get("email") == ""]
    if missing:
        sys.exit(
            f"people.json has no email for: {', '.join(missing)} "
            "(use null for names that are not real members)"
        )

    upload_root = Path(args.upload_path)
    files_root = Path(args.files_to) if args.files_to else upload_root
    db = sqlite3.connect(args.db)
    db.execute("PRAGMA foreign_keys = ON")
    db.row_factory = sqlite3.Row

    group = db.execute(
        "SELECT id FROM groups WHERE id = ?", (args.group,)
    ).fetchone()
    if group is None:
        sys.exit(f"group {args.group} does not exist — create the Loop in the app first")

    existing = db.execute(
        "SELECT COUNT(*) c FROM issues WHERE group_id = ?", (args.group,)
    ).fetchone()["c"]
    if existing and not args.force:
        sys.exit(
            f"group {args.group} already has {existing} issue(s); "
            "import into a fresh Loop or pass --force"
        )

    window = db.execute(
        "SELECT submission_window_days FROM settings WHERE group_id = ?", (args.group,)
    ).fetchone()
    window_days = window["submission_window_days"] if window else 7

    check_month_collisions(db, args.group, bundle["issues"], args.allow_month_collision)

    copies: list[tuple[Path, Path]] = []
    counts = {"issues": 0, "questions": 0, "responses": 0, "blocks": 0, "dump": 0, "users": 0}

    try:
        with db:  # one transaction: all issues land, or none do
            users = resolve_users(db, args.group, people, counts)

            for issue in bundle["issues"]:
                published = datetime.strptime(issue["date"], "%Y-%m-%d").replace(hour=12)
                opens = published - timedelta(days=window_days)
                issue_id = db.execute(
                    """INSERT INTO issues
                       (group_id, title, month, year, status, opens_at, deadline,
                        published_at, count_admin_in, created_at)
                       VALUES (?, ?, ?, ?, 'published', ?, ?, ?, 1, ?)""",
                    (
                        args.group, issue["title"], issue["month"], issue["year"],
                        ts(opens), ts(published), ts(published), ts(opens),
                    ),
                ).lastrowid
                counts["issues"] += 1

                sort = 0
                for section in issue["sections"]:
                    if section["kind"] == "dump":
                        load_dump(
                            db, issue_id, users, section, published,
                            bundle_dir, upload_root, copies, counts,
                        )
                        continue
                    for item in section["items"]:
                        load_question(
                            db, issue_id, users, section, item, sort, published,
                            bundle_dir, upload_root, copies, counts,
                        )
                        sort += 1

            if args.dry_run:
                raise Rollback()
    except Rollback:
        print("dry run: transaction rolled back, no files copied")
        print(summary(counts))
        return 0

    for src, rel in copies:
        dst = files_root / rel
        dst.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(src, dst)

    print(summary(counts) + f", {len(copies)} files -> {files_root}")
    if files_root != upload_root:
        print(f"database paths recorded under {upload_root}")
    return 0


class Rollback(Exception):
    """Raised to abort the load transaction on --dry-run."""


def check_month_collisions(db, group_id: int, issues: list[dict], allow: bool) -> None:
    """Refuse to bury an issue the reading view could never show.

    /issues/{year}/{month} resolves to the first published issue in that
    month, so two issues sharing one month means the older one is reachable
    only through the database. Letterloop's four-weekly cadence produces
    exactly that every 13 issues or so.
    """
    seen: dict[tuple[int, int], list[str]] = {}
    for row in db.execute(
        "SELECT year, month, id FROM issues WHERE group_id = ?", (group_id,)
    ):
        seen.setdefault((row["year"], row["month"]), []).append(f"existing issue {row['id']}")
    for issue in issues:
        seen.setdefault((issue["year"], issue["month"]), []).append(
            f"#{issue['number']} ({issue['date']})"
        )

    clashes = {k: v for k, v in seen.items() if len(v) > 1}
    if not clashes:
        return

    for (year, month), members in sorted(clashes.items()):
        print(f"month collision {year}-{month:02d}: {', '.join(members)}", file=sys.stderr)
    if allow:
        print("--allow-month-collision: importing anyway; only the newest issue "
              "in each month above will be reachable at /issues/YYYY/MM", file=sys.stderr)
        return
    sys.exit(
        "aborting: edit the 'year'/'month' fields of the colliding issues in "
        "bundle.json (published_at keeps the real date), or pass "
        "--allow-month-collision to accept that the older one is unreachable"
    )


def summary(counts: dict) -> str:
    return ", ".join(f"{v} {k}" for k, v in counts.items())


def ts(when: datetime) -> str:
    return when.strftime("%Y-%m-%d %H:%M:%S")


def resolve_users(db, group_id: int, people: dict, counts: dict) -> dict:
    """Map Letterloop display names onto users + memberships."""
    users = {}
    for name, spec in people.items():
        if spec.get("email") is None:
            continue
        email = spec["email"].strip().lower()
        row = db.execute("SELECT id FROM users WHERE email = ?", (email,)).fetchone()
        if row:
            user_id = row["id"]
        else:
            user_id = db.execute(
                "INSERT INTO users (name, email, is_active) VALUES (?, ?, 1)",
                (spec.get("display_name", name), email),
            ).lastrowid
            counts["users"] += 1
        db.execute(
            """INSERT INTO memberships (group_id, user_id, role, is_active)
               VALUES (?, ?, ?, 1)
               ON CONFLICT(group_id, user_id) DO UPDATE SET is_active = 1""",
            (group_id, user_id, spec.get("role", "member")),
        )
        users[name] = user_id
    return users


def load_question(
    db, issue_id, users, section, item, sort, published,
    bundle_dir, upload_root, copies, counts,
):
    asked_by = item.get("asked_by")
    submitted_by = users.get(asked_by) if asked_by else None
    source = section.get("source", "friend" if submitted_by else "admin")
    question_id = db.execute(
        """INSERT INTO questions
           (issue_id, text, source, submitted_by, sort_order, created_at)
           VALUES (?, ?, ?, ?, ?, ?)""",
        (issue_id, item["question"], source, submitted_by, sort, ts(published)),
    ).lastrowid
    counts["questions"] += 1

    for answer in item["answers"]:
        user_id = require_user(users, answer["speaker"])
        response_id = db.execute(
            """INSERT INTO responses
               (user_id, question_id, is_draft, version, created_at, updated_at)
               VALUES (?, ?, 0, 1, ?, ?)""",
            (user_id, question_id, ts(published), ts(published)),
        ).lastrowid
        counts["responses"] += 1

        for order, block in enumerate(answer["blocks"]):
            kind = block["type"]
            if kind == "text":
                db.execute(
                    """INSERT INTO response_blocks
                       (response_id, type, content, sort_order, created_at, updated_at)
                       VALUES (?, 'text', ?, ?, ?, ?)""",
                    (response_id, block["content"], order, ts(published), ts(published)),
                )
            elif kind == "link":
                db.execute(
                    """INSERT INTO response_blocks
                       (response_id, type, link_url, sort_order, created_at, updated_at)
                       VALUES (?, 'link', ?, ?, ?, ?)""",
                    (response_id, block["url"], order, ts(published), ts(published)),
                )
            else:
                dest = stage_media(block, published, bundle_dir, upload_root, copies)
                caption = (
                    VIDEO_STILL_CAPTION.format(duration=block["duration"])
                    if kind == "video_still"
                    else None
                )
                db.execute(
                    """INSERT INTO response_blocks
                       (response_id, type, file_path, caption, sort_order,
                        created_at, updated_at)
                       VALUES (?, 'photo', ?, ?, ?, ?, ?)""",
                    (response_id, str(dest), caption, order, ts(published), ts(published)),
                )
            counts["blocks"] += 1


def load_dump(
    db, issue_id, users, section, published, bundle_dir, upload_root, copies, counts,
):
    """The photo wall becomes the issue's photo & video dump (the collage closer)."""
    for item in section["items"]:
        for answer in item["answers"]:
            user_id = require_user(users, answer["speaker"])
            caption = " ".join(
                b["content"] for b in answer["blocks"] if b["type"] == "text"
            ).strip() or None
            media = [b for b in answer["blocks"] if b["type"] in ("photo", "video_still")]
            for order, block in enumerate(media):
                dest = stage_media(block, published, bundle_dir, upload_root, copies)
                db.execute(
                    """INSERT INTO dump_items
                       (issue_id, user_id, kind, content_type, file_path, caption,
                        sort_order, created_at)
                       VALUES (?, ?, 'photo', ?, ?, ?, ?, ?)""",
                    (
                        issue_id, user_id, f"image/{block['ext']}", str(dest),
                        caption if order == 0 else None, order, ts(published),
                    ),
                )
                counts["dump"] += 1
            if not media and caption:
                # A photo-wall caption with no photo would vanish silently.
                print(f"  warning: dropped photo-wall text without media: {caption[:60]!r}")


def require_user(users: dict, name: str) -> int:
    if name not in users:
        raise SystemExit(f"people.json is missing an entry for {name!r}")
    return users[name]


def stage_media(block, published, bundle_dir, upload_root, copies) -> Path:
    """Return the path to record in the database, queueing the file copy.

    The recorded path is what the app resolves at request time, so under
    Docker it must be the container's UPLOAD_PATH — which is not where the
    bytes go on the host. `copies` carries the destination separately.
    """
    src = bundle_dir / block["file"]
    if not src.exists():
        raise SystemExit(f"missing media file {src}")
    rel = Path(f"{published.year:04d}") / f"{published.month:02d}" / src.name
    copies.append((src, rel))
    return upload_root / rel


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    sub = parser.add_subparsers(dest="cmd", required=True)

    ex = sub.add_parser("extract", help="parse the PDFs into a reviewable bundle")
    ex.add_argument("--pdf-dir", required=True)
    ex.add_argument("--out", required=True)
    ex.set_defaults(func=cmd_extract)

    ld = sub.add_parser("load", help="write a bundle into a PiecesOfLife database")
    ld.add_argument("--bundle", required=True)
    ld.add_argument("--db", required=True)
    ld.add_argument(
        "--upload-path",
        required=True,
        help="UPLOAD_PATH as the app sees it (e.g. /data/uploads in Docker)",
    )
    ld.add_argument(
        "--files-to",
        help="where to actually write the files, if that differs from "
             "--upload-path (e.g. the host side of the uploads volume)",
    )
    ld.add_argument("--group", type=int, default=1)
    ld.add_argument("--force", action="store_true", help="import into a non-empty Loop")
    ld.add_argument(
        "--allow-month-collision",
        action="store_true",
        help="import even when two issues share a month (older one unreachable)",
    )
    ld.add_argument("--dry-run", action="store_true")
    ld.set_defaults(func=cmd_load)

    args = parser.parse_args()
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
