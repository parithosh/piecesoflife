/**
 * Next-issue question suggestion box with inline @mention targeting.
 *
 * Shared by the published issue reading view and the signed-in archive
 * landing page. Typing "@" (or clicking the "@ Ask one person" button) opens
 * a keyboard-accessible picker of active circle members; picking someone
 * inserts "@Name " inline in the textarea and attaches target_user_id on
 * submit so only that member answers while everyone reads their response in
 * "The hot seat".
 */
(function () {
    'use strict';

    function initSuggestForm(form) {
        const textEl = form.querySelector('[data-suggest-text]');
        const statusEl = form.querySelector('[data-suggest-status]');
        const menuEl = form.querySelector('[data-mention-menu]');
        const triggerBtn = form.querySelector('[data-mention-trigger]');
        if (!textEl || !statusEl) return;

        let members = [];
        try {
            members = JSON.parse(form.dataset.members || '[]') || [];
        } catch {
            members = [];
        }

        let pickedMember = null; // last member picked via dropdown
        let mentionRange = null; // { start, end } in textEl.value
        let filtered = [];
        let activeIdx = 0;

        function closeMenu() {
            mentionRange = null;
            filtered = [];
            activeIdx = 0;
            if (menuEl) {
                menuEl.hidden = true;
                menuEl.replaceChildren();
            }
            textEl.setAttribute('aria-expanded', 'false');
        }

        function renderMenu() {
            if (!menuEl) return;
            menuEl.replaceChildren();
            if (!filtered.length) {
                const empty = document.createElement('div');
                empty.className = 'pl-mention-empty';
                empty.textContent = 'No matching circle member';
                menuEl.appendChild(empty);
                menuEl.hidden = false;
                textEl.setAttribute('aria-expanded', 'true');
                return;
            }

            filtered.forEach((m, idx) => {
                const btn = document.createElement('button');
                btn.type = 'button';
                btn.className = 'pl-mention-item' + (idx === activeIdx ? ' is-active' : '');
                btn.setAttribute('role', 'option');
                btn.setAttribute('aria-selected', idx === activeIdx ? 'true' : 'false');

                const ava = document.createElement('span');
                ava.className = 'pl-mention-ava';
                ava.setAttribute('aria-hidden', 'true');
                if (m.avatar_url) {
                    const img = document.createElement('img');
                    img.src = m.avatar_url;
                    img.alt = '';
                    ava.appendChild(img);
                } else {
                    ava.textContent = (m.name || '?').trim().charAt(0).toUpperCase();
                }

                const label = document.createElement('span');
                label.className = 'pl-mention-name';
                label.textContent = m.name;

                btn.append(ava, label);
                btn.addEventListener('mousedown', (ev) => {
                    ev.preventDefault();
                    pickMember(m);
                });
                menuEl.appendChild(btn);
            });

            menuEl.hidden = false;
            textEl.setAttribute('aria-expanded', 'true');
        }

        function pickMember(member) {
            if (mentionRange) {
                const before = textEl.value.slice(0, mentionRange.start);
                const after = textEl.value.slice(mentionRange.end);
                const mention = '@' + member.name;
                const spacer = after.startsWith(' ') || after.startsWith(',') || after.startsWith('?') ? '' : ' ';
                const nextVal = before + mention + spacer + after;
                const caret = (before + mention + spacer).length;
                textEl.value = nextVal;
                textEl.setSelectionRange(caret, caret);
            }
            pickedMember = member;
            closeMenu();
            textEl.focus();
        }

        function detectMention() {
            if (!members.length || !menuEl) return;
            const pos = textEl.selectionStart;
            if (pos == null || pos !== textEl.selectionEnd) {
                closeMenu();
                return;
            }

            const before = textEl.value.slice(0, pos);
            // Match "@" at start or after whitespace, followed by up to 40 chars
            // without another "@" or newline.
            const m = before.match(/(^|\s)@([^\s@\n]{0,40})$/);
            if (!m) {
                closeMenu();
                return;
            }

            const start = pos - m[2].length - 1;
            const query = m[2].toLowerCase();
            mentionRange = { start, end: pos };
            filtered = members.filter(mem =>
                (mem.name || '').toLowerCase().includes(query)
            ).slice(0, 8);
            activeIdx = 0;
            renderMenu();
        }

        textEl.addEventListener('input', detectMention);
        textEl.addEventListener('click', detectMention);
        textEl.addEventListener('blur', () => {
            // Defer slightly so mousedown on a menu option runs first.
            setTimeout(closeMenu, 120);
        });

        textEl.addEventListener('keydown', (ev) => {
            if (!mentionRange || !menuEl || menuEl.hidden) return;
            if (ev.key === 'ArrowDown') {
                if (!filtered.length) return;
                ev.preventDefault();
                activeIdx = (activeIdx + 1) % filtered.length;
                renderMenu();
            } else if (ev.key === 'ArrowUp') {
                if (!filtered.length) return;
                ev.preventDefault();
                activeIdx = (activeIdx - 1 + filtered.length) % filtered.length;
                renderMenu();
            } else if (ev.key === 'Enter' || ev.key === 'Tab') {
                if (!filtered.length) return;
                ev.preventDefault();
                pickMember(filtered[activeIdx]);
            } else if (ev.key === 'Escape') {
                ev.preventDefault();
                closeMenu();
            }
        });

        if (triggerBtn && members.length) {
            triggerBtn.addEventListener('click', () => {
                textEl.focus();
                const pos = textEl.selectionStart ?? textEl.value.length;
                const before = textEl.value.slice(0, pos);
                const after = textEl.value.slice(pos);
                const needsSpace = before.length > 0 && !/\s$/.test(before);
                const insert = (needsSpace ? ' ' : '') + '@';
                textEl.value = before + insert + after;
                const nextPos = pos + insert.length;
                textEl.setSelectionRange(nextPos, nextPos);
                detectMention();
            });
        }

        function escapeRegExp(s) {
            return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
        }

        // Resolve an inline "@Name" mention anywhere in the question text.
        // Leading "@Name " is stripped from the stored question text (since
        // The hot seat already labels it "For Name"), while mid-sentence
        // mentions stay in prose with the leading "@" dropped.
        function resolveInlineMention(raw) {
            if (!members.length) {
                return { text: raw, target: null };
            }

            const candidates = [];
            if (pickedMember) candidates.push(pickedMember);
            const sorted = [...members].sort((a, b) => (b.name || '').length - (a.name || '').length);
            for (const m of sorted) {
                if (!pickedMember || m.id !== pickedMember.id) candidates.push(m);
            }

            for (const m of candidates) {
                const name = (m.name || '').trim();
                if (!name) continue;
                const re = new RegExp('(^|\\s)@' + escapeRegExp(name) + '(?=$|[\\s,:;.!?—–-])', 'i');
                const match = raw.match(re);
                if (!match) continue;

                const idx = match.index;
                if (idx === 0) {
                    const cleaned = raw.slice(match[0].length).replace(/^[\s,:—–-]+/, '').trim();
                    return { text: cleaned, target: m };
                }

                // Mid-sentence mention: replace "@Name" with "Name" for clean reading.
                const cleaned = (
                    raw.slice(0, idx) +
                    match[1] + name +
                    raw.slice(idx + match[0].length)
                ).trim();
                return { text: cleaned, target: m };
            }

            return { text: raw, target: null };
        }

        form.addEventListener('submit', async function (ev) {
            ev.preventDefault();
            closeMenu();

            const raw = textEl.value.trim();
            if (!raw) return;

            const resolved = resolveInlineMention(raw);
            if (!resolved.text) {
                statusEl.style.color = 'var(--sindoor)';
                statusEl.textContent = resolved.target
                    ? `Add a question after @${resolved.target.name}`
                    : 'Please enter a question';
                textEl.focus();
                return;
            }

            const csrf = getCSRFToken();
            statusEl.style.color = 'var(--ink-soft)';
            statusEl.textContent = 'Sending…';

            const payload = {
                issue_id: Number(form.dataset.issueId),
                text: resolved.text,
            };
            if (resolved.target) {
                payload.target_user_id = Number(resolved.target.id);
            }

            try {
                const res = await fetch('/api/questions/submit', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
                    body: JSON.stringify(payload),
                });
                const data = await res.json().catch(() => ({}));

                if (res.ok) {
                    textEl.value = '';
                    const sentFor = resolved.target ? resolved.target.name : null;
                    pickedMember = null;
                    statusEl.style.color = 'var(--peacock)';
                    statusEl.textContent = sentFor
                        ? `Sent for ${sentFor} — it'll be in The hot seat next round ✓`
                        : "Sent — it'll be in the next round ✓";
                } else {
                    statusEl.style.color = 'var(--sindoor)';
                    statusEl.textContent = data?.error?.message || 'Could not send';
                }
            } catch {
                statusEl.style.color = 'var(--sindoor)';
                statusEl.textContent = 'Network error';
            }
        });
    }

    document.querySelectorAll('[data-suggest-form]').forEach(initSuggestForm);
})();
