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
        const submitBtn = form.querySelector('button[type="submit"]');
        if (!textEl || !statusEl) return;

        let members = [];
        try {
            members = JSON.parse(form.dataset.members || '[]') || [];
        } catch {
            members = [];
        }

        let pickedMember = null; // explicitly chosen from the @ picker
        let mentionRange = null; // { start, end } in textEl.value
        let filtered = [];
        let activeIdx = 0;
        let blurTimer = null;
        let submitting = false;
        let showingTargetHint = false;

        function escapeRegExp(s) {
            return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
        }

        // Unicode-aware boundary so "@Priya's", "@Priya)", "@Priya…", etc.
        // all match cleanly without swallowing prefixes of longer words.
        function mentionPatternFor(name, leadingOnly) {
            const prefix = leadingOnly ? '^' : '(^|\\s)';
            return new RegExp(prefix + '@' + escapeRegExp(name) + '(?=$|[^\\p{L}\\p{N}])', 'iu');
        }

        function updateTargetHint() {
            const res = resolveMention(textEl.value.trim());
            if (res.target && !res.error) {
                statusEl.style.color = 'var(--peacock)';
                statusEl.textContent = `Only ${res.target.name} will answer — read by everyone in The hot seat`;
                showingTargetHint = true;
            } else if (showingTargetHint) {
                statusEl.textContent = '';
                showingTargetHint = false;
            }
        }

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
                    clearTimeout(blurTimer);
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
                const spacer = after.startsWith(' ') || /^[,;.!?')\]—–-]/.test(after) ? '' : ' ';
                const nextVal = before + mention + spacer + after;
                const caret = (before + mention + spacer).length;
                textEl.value = nextVal;
                textEl.setSelectionRange(caret, caret);
            }
            pickedMember = member;
            closeMenu();
            updateTargetHint();
            textEl.focus();
        }

        function syncPickedMember() {
            if (!pickedMember) return;
            const name = (pickedMember.name || '').trim();
            if (!name || !mentionPatternFor(name, false).test(textEl.value)) {
                pickedMember = null;
            }
        }

        function detectMention() {
            syncPickedMember();
            updateTargetHint();
            if (!members.length || !menuEl) return;
            const pos = textEl.selectionStart;
            if (pos == null || pos !== textEl.selectionEnd) {
                closeMenu();
                return;
            }

            const before = textEl.value.slice(0, pos);
            // Match "@" at start or after whitespace, followed by up to 40 chars
            // without another "@" or newline (allows spaces for multi-word names
            // only while filtering if no member was already finalized).
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
            clearTimeout(blurTimer);
            // Defer slightly so mousedown on a menu option or trigger runs first.
            blurTimer = setTimeout(closeMenu, 120);
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
            // Prevent blur timer from collapsing the menu right after opening.
            triggerBtn.addEventListener('mousedown', (ev) => {
                ev.preventDefault();
                clearTimeout(blurTimer);
            });
            triggerBtn.addEventListener('click', () => {
                clearTimeout(blurTimer);
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

        // Resolve targeting cleanly without silent misrouting:
        //  1. If the user explicitly picked a member from the dropdown and
        //     "@Name" is still present, target that exact member ID.
        //  2. Otherwise, check only for a leading "@Name" at the start of the
        //     prompt (longest name first), rejecting ambiguous duplicate names.
        //  3. If any unresolved "@word" remains in the text (e.g. typo, partial
        //     multi-word name, or unpicked mid-sentence mention), block submit
        //     with a clear hint instead of silently broadcasting or misrouting.
        function resolveMention(raw) {
            if (!members.length) {
                return { text: raw, target: null, error: null };
            }

            let target = null;
            let cleaned = raw;

            if (pickedMember) {
                const name = (pickedMember.name || '').trim();
                if (name) {
                    const m = raw.match(mentionPatternFor(name, false));
                    if (m) {
                        target = pickedMember;
                        if (m.index === 0) {
                            cleaned = raw.slice(m[0].length).replace(/^[\s,:—–-]+/, '').trim();
                        } else {
                            cleaned = (
                                raw.slice(0, m.index) +
                                (m[1] || '') + name +
                                raw.slice(m.index + m[0].length)
                            ).trim();
                        }
                    }
                }
            }

            if (!target && raw.startsWith('@')) {
                const sorted = [...members].sort((a, b) => (b.name || '').length - (a.name || '').length);
                const matches = [];
                let bestLen = 0;
                for (const m of sorted) {
                    const name = (m.name || '').trim();
                    if (!name) continue;
                    const hit = raw.match(mentionPatternFor(name, true));
                    if (!hit) continue;
                    if (name.length > bestLen) {
                        bestLen = name.length;
                        matches.length = 0;
                        matches.push({ member: m, match: hit });
                    } else if (name.length === bestLen) {
                        matches.push({ member: m, match: hit });
                    }
                }
                if (matches.length > 1) {
                    return {
                        text: raw,
                        target: null,
                        error: 'Multiple members share that name — pick one from the @ menu',
                    };
                }
                if (matches.length === 1) {
                    target = matches[0].member;
                    cleaned = raw.slice(matches[0].match[0].length).replace(/^[\s,:—–-]+/, '').trim();
                }
            }

            // Catch any remaining "@word" mention (typo, partial name, second
            // @mention, or unpicked mid-sentence @mention) so nothing is
            // silently sent to the wrong audience.
            if (/(^|\s)@[\p{L}\p{N}_]/u.test(cleaned)) {
                return {
                    text: cleaned,
                    target: null,
                    error: target
                        ? 'You can only put one person in The hot seat per question'
                        : 'Pick a person from the @ menu, or remove the @ to ask everyone',
                };
            }

            return { text: cleaned, target, error: null };
        }

        form.addEventListener('submit', async function (ev) {
            ev.preventDefault();
            if (submitting) return;
            closeMenu();

            const submittedValue = textEl.value;
            const raw = submittedValue.trim();
            if (!raw) return;

            syncPickedMember();
            const resolved = resolveMention(raw);
            if (resolved.error) {
                showingTargetHint = false;
                statusEl.style.color = 'var(--sindoor)';
                statusEl.textContent = resolved.error;
                textEl.focus();
                return;
            }
            if (!resolved.text) {
                showingTargetHint = false;
                statusEl.style.color = 'var(--sindoor)';
                statusEl.textContent = resolved.target
                    ? `Add a question after @${resolved.target.name}`
                    : 'Please enter a question';
                textEl.focus();
                return;
            }

            submitting = true;
            if (submitBtn) submitBtn.disabled = true;
            showingTargetHint = false;
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
                    if (textEl.value === submittedValue) {
                        textEl.value = '';
                        pickedMember = null;
                    }
                    const sentFor = resolved.target ? resolved.target.name : null;
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
            } finally {
                submitting = false;
                if (submitBtn) submitBtn.disabled = false;
            }
        });
    }

    document.querySelectorAll('[data-suggest-form]').forEach(initSuggestForm);
})();
