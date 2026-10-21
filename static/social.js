/**
 * Collapsed comments widget for the published issue view.
 */

(function() {
    const ALLOWED_COMMENT_TAGS = new Set([
        'p', 'br', 'strong', 'em', 'code', 'pre', 'blockquote',
        'ul', 'ol', 'li', 'a',
    ]);

    // Panels target a response, a single notebook (diary) block, a legacy
    // notebook day (issues published before per-block threads), or a dump
    // item; the data attribute decides which comments API the panel talks to.
    const PANEL_SELECTOR =
        '.comments-panel[data-response-id], .comments-panel[data-diary-block-id], ' +
        '.comments-panel[data-diary-day-id], .comments-panel[data-dump-item-id]';

    // Set by the issue page when the server has GIPHY_API_KEY; without it
    // composers have no GIF button.
    const GIPHY_KEY = window.POLGiphyKey || '';

    function gifButtonHTML() {
        if (!GIPHY_KEY) return '';
        return '<button type="button" class="comment-gif-btn" aria-expanded="false">GIF</button>';
    }

    function commentsEndpoint(panel) {
        if (panel.dataset.responseId) {
            return `/api/responses/${panel.dataset.responseId}/comments`;
        }
        if (panel.dataset.diaryBlockId) {
            return `/api/diary-blocks/${panel.dataset.diaryBlockId}/comments`;
        }
        if (panel.dataset.diaryDayId) {
            return `/api/diary-days/${panel.dataset.diaryDayId}/comments`;
        }
        return `/api/dump/${panel.dataset.dumpItemId}/comments`;
    }

    // CSRF helpers (getCSRFToken/apiHeaders) come from the base layout.

    function commentLabel(count) {
        if (count > 0) return `${count} ${count === 1 ? 'comment' : 'comments'}`;
        return 'add a comment';
    }

    function setupPanel(panel) {
        if (panel.dataset.ready === '1') return;
        panel.dataset.ready = '1';

        const count = Number(panel.dataset.commentCount || 0);
        // Every thread starts expanded — the conversation (or the empty
        // composer) is visible without a click; the toggle only collapses.
        panel.innerHTML = `
            <button type="button" class="comment-toggle ${count > 0 ? 'has-comments' : ''}" aria-expanded="true">
                <span class="comment-toggle-icon">▾</span>
                <span class="comment-toggle-label">${commentLabel(count)}</span>
            </button>
            <div class="comment-thread">
                <ul class="comment-list"></ul>
                <form class="comment-form">
                    <textarea class="comment-body-input" placeholder="Add a comment..."
                              rows="1" maxlength="4000"></textarea>
                    <div class="comment-form-actions">
                        <span class="comment-status"></span>
                        <span>
                            ${gifButtonHTML()}
                            <button type="submit" disabled>Post</button>
                        </span>
                    </div>
                </form>
            </div>
        `;

        if (count > 0) {
            loadComments(panel);
        } else {
            panel.dataset.loaded = '1';
        }
    }

    async function loadComments(panel) {
        try {
            const res = await fetch(commentsEndpoint(panel));
            if (!res.ok) throw new Error(`comments fetch failed: ${res.status}`);
            const data = await res.json();
            renderComments(panel, data.comments || []);
            panel.dataset.loaded = '1';
        } catch {
            const list = panel.querySelector('.comment-list');
            if (list && panel.dataset.loaded !== '1') {
                list.innerHTML =
                    '<li class="comment-item comment-load-error">Couldn&#39;t load comments — check your connection and reload.</li>';
            }
        }
    }

    function commentItemHTML(c, replies) {
        // Replies always target the top-level comment so the thread stays
        // one level deep no matter which comment the user replies to.
        const replyTarget = c.parent_id || c.id;
        const mine = window.POLUserID && c.user_id === window.POLUserID;
        return `
            <li class="comment-item" data-comment-id="${c.id}"
                data-raw-body="${escapeHtml(c.body || '')}">
                <div class="comment-meta">
                    <strong>${escapeHtml(c.author_name)}</strong>
                    <span>${formatTimestamp(c.created_at)}</span>
                    ${c.edited_at ? '<span class="comment-edited">(edited)</span>' : ''}
                </div>
                <div class="comment-body">${safeCommentHTML(c)}</div>
                <button type="button" class="comment-reply-btn"
                        data-reply-to="${replyTarget}"
                        aria-label="Reply to ${escapeHtml(c.author_name)}">↳ reply</button>
                ${mine ? `
                <button type="button" class="comment-reply-btn comment-edit-btn"
                        data-edit-comment="${c.id}"
                        aria-label="Edit your comment">✎ edit</button>` : ''}
                ${replies && replies.length ? `
                <ul class="comment-replies">
                    ${replies.map(r => commentItemHTML(r, null)).join('')}
                </ul>` : ''}
            </li>
        `;
    }

    // closeEditForms removes every open edit composer and unhides the
    // comment bodies they were covering — one edit at a time, so an
    // unsaved draft is never silently lost behind another form's save.
    function closeEditForms() {
        document.querySelectorAll('.comment-edit-form').forEach(f => {
            const it = f.closest('.comment-item');
            it?.querySelector(':scope > .comment-body')?.removeAttribute('hidden');
            f.remove();
        });
    }

    function openEditForm(panel, btn) {
        const item = btn.closest('.comment-item');
        if (!item) return;

        // Clicking edit on an already-open comment closes its composer.
        const wasOpen = item.querySelector(':scope > .comment-edit-form');
        removeReplyForm(document);
        closeEditForms();
        if (wasOpen) return;

        const body = item.querySelector(':scope > .comment-body');
        const form = document.createElement('form');
        form.className = 'comment-form comment-edit-form has-content';
        form.dataset.editId = btn.dataset.editComment;
        form.innerHTML = `
            <textarea class="comment-body-input" rows="2" maxlength="4000"></textarea>
            <div class="comment-form-actions">
                <span class="comment-status"></span>
                <span>
                    <button type="button" class="comment-edit-cancel">Cancel</button>
                    <button type="submit">Save</button>
                </span>
            </div>
        `;
        form.querySelector('.comment-body-input').value = item.dataset.rawBody || '';
        body.hidden = true;
        item.insertBefore(form, body.nextSibling);
        form.querySelector('.comment-body-input')?.focus();
    }

    async function saveEdit(form) {
        const panel = form.closest(PANEL_SELECTOR);
        const textarea = form.querySelector('.comment-body-input');
        const status = form.querySelector('.comment-status');
        const body = textarea.value.trim();
        if (!panel || !body) return;

        status.textContent = 'Saving...';
        status.dataset.state = 'pending';

        try {
            const res = await fetch(`/api/comments/${form.dataset.editId}`, {
                method: 'PATCH',
                headers: apiHeaders(),
                body: JSON.stringify({ body }),
            });

            if (res.ok) {
                await loadComments(panel);
                return;
            }

            const data = await res.json().catch(() => ({}));
            status.textContent = data?.error?.message || 'Failed';
            status.dataset.state = 'error';
        } catch {
            status.textContent = 'Network error';
            status.dataset.state = 'error';
        }
    }

    function renderComments(panel, comments) {
        const list = panel.querySelector('.comment-list');
        if (!list) return;

        const byParent = new Map();
        const topLevel = [];
        comments.forEach(c => {
            if (c.parent_id) {
                const siblings = byParent.get(c.parent_id) || [];
                siblings.push(c);
                byParent.set(c.parent_id, siblings);
            } else {
                topLevel.push(c);
            }
        });

        // Replies whose parent is missing (deleted) fall back to top level.
        byParent.forEach((orphans, parentID) => {
            if (!topLevel.some(c => c.id === parentID)) topLevel.push(...orphans);
        });

        list.innerHTML = topLevel.map(c =>
            commentItemHTML(c, byParent.get(c.id))
        ).join('');

        panel.dataset.commentCount = String(comments.length);
        updateToggle(panel, comments.length);
    }

    function removeReplyForm(scope) {
        (scope || document).querySelectorAll('.comment-reply-form').forEach(f => f.remove());
    }

    function openReplyForm(panel, btn) {
        const item = btn.closest('.comment-item');
        if (!item) return;

        // One composer at a time; clicking reply again closes it.
        const existing = item.querySelector(':scope > .comment-reply-form');
        removeReplyForm(document);
        closeEditForms();
        if (existing) return;

        const form = document.createElement('form');
        form.className = 'comment-form comment-reply-form';
        form.dataset.parentId = btn.dataset.replyTo;
        form.innerHTML = `
            <textarea class="comment-body-input" placeholder="Write a reply..."
                      rows="1" maxlength="4000"></textarea>
            <div class="comment-form-actions">
                <span class="comment-status"></span>
                <span>
                    <button type="button" class="comment-reply-cancel">Cancel</button>
                    ${gifButtonHTML()}
                    <button type="submit" disabled>Reply</button>
                </span>
            </div>
        `;
        // Insert after the reply button, before any nested replies, so the
        // composer reads as part of this comment.
        const replies = item.querySelector(':scope > .comment-replies');
        item.insertBefore(form, replies || null);
        form.querySelector('.comment-body-input')?.focus();
    }

    function updateToggle(panel, count) {
        const btn = panel.querySelector('.comment-toggle');
        if (!btn) return;

        const expanded = btn.getAttribute('aria-expanded') === 'true';
        const icon = btn.querySelector('.comment-toggle-icon');
        const label = btn.querySelector('.comment-toggle-label');
        btn.classList.toggle('has-comments', count > 0);
        if (icon) icon.textContent = count > 0 ? '▾' : (expanded ? '▾' : '▸');
        if (label) label.textContent = commentLabel(count);
    }

    async function togglePanel(panel) {
        const btn = panel.querySelector('.comment-toggle');
        const thread = panel.querySelector('.comment-thread');
        if (!btn || !thread) return;

        const expanded = btn.getAttribute('aria-expanded') === 'true';
        btn.setAttribute('aria-expanded', expanded ? 'false' : 'true');
        thread.hidden = expanded;

        if (!expanded && panel.dataset.loaded !== '1') {
            await loadComments(panel);
        }

        updateToggle(panel, Number(panel.dataset.commentCount || 0));

        if (!expanded && Number(panel.dataset.commentCount || 0) === 0) {
            panel.querySelector('.comment-body-input')?.focus();
        }
    }

    async function postComment(form) {
        const panel = form.closest(PANEL_SELECTOR);
        if (!panel) return;

        const textarea = form.querySelector('.comment-body-input');
        const status = form.querySelector('.comment-status');
        const submit = form.querySelector('button[type="submit"]');
        const body = commentBodyFor(form);
        if (!body) return;

        status.textContent = 'Posting...';
        status.dataset.state = 'pending';
        submit.disabled = true;

        const payload = { body };
        const parentID = Number(form.dataset.parentId || 0);
        if (parentID > 0) payload.parent_id = parentID;

        try {
            const res = await fetch(commentsEndpoint(panel), {
                method: 'POST',
                headers: apiHeaders(),
                body: JSON.stringify(payload),
            });

            if (res.ok) {
                textarea.value = '';
                detachGif(form);
                status.textContent = '';
                status.dataset.state = '';
                await loadComments(panel);
                return;
            }

            const data = await res.json().catch(() => ({}));
            status.textContent = data?.error?.message || 'Failed';
            status.dataset.state = 'error';
        } catch {
            status.textContent = 'Network error';
            status.dataset.state = 'error';
        } finally {
            syncFormState(form);
        }
    }

    // syncFormState keeps a composer's has-content class (which keeps it
    // expanded) and its submit button in step with its text and GIF.
    function syncFormState(form) {
        const textarea = form.querySelector('.comment-body-input');
        const hasContent = textarea.value.trim().length > 0 || Boolean(form.dataset.gifUrl);
        form.classList.toggle('has-content', hasContent);
        const submit = form.querySelector('button[type="submit"]');
        if (submit) submit.disabled = !hasContent;
    }

    // commentBodyFor is the Markdown a composer posts: its text, then the
    // attached GIF as an image on its own paragraph.
    function commentBodyFor(form) {
        const text = form.querySelector('.comment-body-input').value.trim();
        const url = form.dataset.gifUrl;
        if (!url) return text;

        const alt = (form.dataset.gifTitle || '').replace(/[[\]\\\r\n]/g, '').trim() || 'GIF';
        // Percent-encode what would end a Markdown link target early.
        const target = url.replace(/[()\s]/g, c => '%' + c.charCodeAt(0).toString(16).padStart(2, '0'));
        const gif = `![${alt}](${target})`;
        return text ? `${text}\n\n${gif}` : gif;
    }

    function toggleGifPicker(form, btn) {
        if (form.querySelector('.gif-picker')) {
            closeGifPicker(form);
            return;
        }

        const picker = document.createElement('div');
        picker.className = 'gif-picker';
        picker.innerHTML = `
            <input type="search" class="gif-search" placeholder="Search GIPHY…"
                   aria-label="Search GIFs" maxlength="50" autocomplete="off">
            <div class="gif-results"></div>
            <p class="gif-attribution">Powered by GIPHY</p>
        `;
        form.querySelector('.comment-form-actions').before(picker);
        btn.setAttribute('aria-expanded', 'true');
        picker.querySelector('.gif-search').focus();
    }

    function closeGifPicker(form) {
        form.querySelector('.gif-picker')?.remove();
        form.querySelector('.comment-gif-btn')?.setAttribute('aria-expanded', 'false');
    }

    function gifNote(text) {
        return `<p class="gif-note">${escapeHtml(text)}</p>`;
    }

    // searchGifs queries GIPHY from the browser, as GIPHY requires. Each
    // search is numbered so a slow earlier response can't replace newer
    // results.
    async function searchGifs(picker) {
        const q = picker.querySelector('.gif-search').value.trim();
        const results = picker.querySelector('.gif-results');
        const seq = String(Number(picker.dataset.seq || 0) + 1);
        picker.dataset.seq = seq;

        if (q.length < 2) {
            results.innerHTML = '';
            return;
        }

        results.innerHTML = gifNote('Searching…');

        const params = new URLSearchParams({
            api_key: GIPHY_KEY, q, limit: '12', rating: 'pg-13',
        });
        let html;
        try {
            const res = await fetch(`https://api.giphy.com/v1/gifs/search?${params}`, {
                referrerPolicy: 'no-referrer',
            });
            if (res.status === 429) {
                html = gifNote('That’s a lot of GIF searches this hour — try again later.');
            } else if (!res.ok) {
                throw new Error(`GIPHY search failed: ${res.status}`);
            } else {
                const data = await res.json();
                html = (data.data || []).map(gifResultHTML).join('') || gifNote('No GIFs found.');
            }
        } catch {
            html = gifNote('GIF search isn’t available right now.');
        }

        if (picker.dataset.seq === seq) results.innerHTML = html;
    }

    // gifResultHTML shows a small rendition in the picker; the comment gets
    // the 200px-tall one. WebP where GIPHY has it — far smaller than GIF.
    function gifResultHTML(g) {
        const full = g.images?.fixed_height;
        const thumb = g.images?.fixed_width_small || full;
        const url = full?.webp || full?.url;
        const preview = thumb?.webp || thumb?.url;
        if (!isGiphyMediaURL(url) || !isGiphyMediaURL(preview)) return '';

        const title = g.title || 'GIF';
        return `
            <button type="button" class="gif-result" title="${escapeHtml(title)}"
                    data-gif-url="${escapeHtml(url)}" data-gif-title="${escapeHtml(title)}">
                <img src="${escapeHtml(preview)}" alt="${escapeHtml(title)}"
                     loading="lazy" referrerpolicy="no-referrer">
            </button>
        `;
    }

    function attachGif(form, result) {
        form.dataset.gifUrl = result.dataset.gifUrl;
        form.dataset.gifTitle = result.dataset.gifTitle;
        closeGifPicker(form);

        form.querySelector('.gif-attached')?.remove();
        const attached = document.createElement('div');
        attached.className = 'gif-attached';
        attached.innerHTML = `
            <img src="${escapeHtml(form.dataset.gifUrl)}" alt="${escapeHtml(form.dataset.gifTitle)}"
                 referrerpolicy="no-referrer">
            <button type="button" class="gif-remove" aria-label="Remove GIF">×</button>
        `;
        form.querySelector('.comment-form-actions').before(attached);
        syncFormState(form);
        form.querySelector('.comment-body-input')?.focus();
    }

    function detachGif(form) {
        delete form.dataset.gifUrl;
        delete form.dataset.gifTitle;
        form.querySelector('.gif-attached')?.remove();
        closeGifPicker(form);
        syncFormState(form);
    }

    function escapeHtml(s) {
        return String(s ?? '')
            .replace(/&/g, '&amp;').replace(/</g, '&lt;')
            .replace(/>/g, '&gt;').replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;');
    }

    function safeCommentHTML(comment) {
        if (!comment.body_html) return escapeHtml(comment.body);

        const template = document.createElement('template');
        template.innerHTML = String(comment.body_html);
        sanitizeCommentFragment(template.content);
        return template.innerHTML;
    }

    function sanitizeCommentFragment(root) {
        Array.from(root.childNodes).forEach(node => {
            if (node.nodeType === Node.TEXT_NODE) return;

            if (node.nodeType !== Node.ELEMENT_NODE) {
                node.remove();
                return;
            }

            const tag = node.tagName.toLowerCase();
            if (tag === 'img') {
                sanitizeCommentImage(node);
                return;
            }

            if (!ALLOWED_COMMENT_TAGS.has(tag)) {
                node.replaceWith(document.createTextNode(node.textContent || ''));
                return;
            }

            Array.from(node.attributes).forEach(attr => {
                const name = attr.name.toLowerCase();
                if (tag === 'a' && name === 'href' && isSafeCommentHref(attr.value)) {
                    return;
                }
                node.removeAttribute(attr.name);
            });

            if (tag === 'a') {
                node.setAttribute('rel', 'noopener noreferrer');
                node.setAttribute('target', '_blank');
            }

            sanitizeCommentFragment(node);
        });
    }

    // Comment images exist only as GIFs from the picker, so only GIPHY's
    // media hosts may supply one; anything else is dropped. Survivors keep
    // just src and alt, and don't tell GIPHY which site they're on.
    function sanitizeCommentImage(img) {
        const src = img.getAttribute('src') || '';
        if (!isGiphyMediaURL(src)) {
            img.remove();
            return;
        }

        const alt = img.getAttribute('alt') || 'GIF';
        Array.from(img.attributes).forEach(attr => img.removeAttribute(attr.name));
        img.setAttribute('src', src);
        img.setAttribute('alt', alt);
        img.setAttribute('class', 'comment-gif');
        img.setAttribute('loading', 'lazy');
        img.setAttribute('referrerpolicy', 'no-referrer');
    }

    function isGiphyMediaURL(src) {
        try {
            const url = new URL(src);
            return url.protocol === 'https:' && /^(media\d*|i)\.giphy\.com$/.test(url.hostname);
        } catch {
            return false;
        }
    }

    function isSafeCommentHref(href) {
        try {
            const url = new URL(href, window.location.origin);
            return ['http:', 'https:', 'mailto:'].includes(url.protocol);
        } catch {
            return false;
        }
    }

    function formatTimestamp(iso) {
        try {
            const d = new Date(iso);
            const now = new Date();
            const diffMs = now - d;
            const diffMin = Math.floor(diffMs / 60000);
            if (diffMin < 1) return 'just now';
            if (diffMin < 60) return `${diffMin}m ago`;
            const diffHr = Math.floor(diffMin / 60);
            if (diffHr < 24) return `${diffHr}h ago`;
            const diffDay = Math.floor(diffHr / 24);
            if (diffDay < 7) return `${diffDay}d ago`;
            return d.toLocaleDateString('en-GB'); // DD/MM/YYYY
        } catch {
            return iso;
        }
    }

    function init(root) {
        const scope = root || document;
        scope.querySelectorAll(PANEL_SELECTOR).forEach(setupPanel);
    }

    document.addEventListener('click', ev => {
        const gifBtn = ev.target.closest('.comment-gif-btn');
        if (gifBtn) {
            toggleGifPicker(gifBtn.closest('.comment-form'), gifBtn);
            return;
        }

        const gifResult = ev.target.closest('.gif-result');
        if (gifResult) {
            attachGif(gifResult.closest('.comment-form'), gifResult);
            return;
        }

        const gifRemove = ev.target.closest('.gif-remove');
        if (gifRemove) {
            const form = gifRemove.closest('.comment-form');
            detachGif(form);
            form.querySelector('.comment-body-input')?.focus();
            return;
        }

        const editBtn = ev.target.closest('.comment-edit-btn');
        if (editBtn) {
            const panel = editBtn.closest(PANEL_SELECTOR);
            if (panel) openEditForm(panel, editBtn);
            return;
        }

        if (ev.target.closest('.comment-edit-cancel')) {
            closeEditForms();
            return;
        }

        const replyBtn = ev.target.closest('.comment-reply-btn:not(.comment-edit-btn)');
        if (replyBtn) {
            const panel = replyBtn.closest(PANEL_SELECTOR);
            if (panel) openReplyForm(panel, replyBtn);
            return;
        }

        if (ev.target.closest('.comment-reply-cancel')) {
            removeReplyForm(document);
            return;
        }

        const btn = ev.target.closest('.comment-toggle');
        if (!btn) return;

        const panel = btn.closest(PANEL_SELECTOR);
        if (panel) togglePanel(panel);
    });

    document.addEventListener('input', ev => {
        const search = ev.target.closest('.gif-search');
        if (search) {
            // Wait for a pause in typing: the free GIPHY key allows only
            // 100 searches an hour for the whole circle.
            const picker = search.closest('.gif-picker');
            clearTimeout(picker.searchTimer);
            picker.searchTimer = setTimeout(() => searchGifs(picker), 400);
            return;
        }

        const form = ev.target.closest('.comment-body-input')?.closest('.comment-form');
        if (form) syncFormState(form);
    });

    document.addEventListener('keydown', ev => {
        const search = ev.target.closest?.('.gif-search');
        if (!search) return;

        const picker = search.closest('.gif-picker');
        if (ev.key === 'Enter') {
            // Search now instead of submitting the comment.
            ev.preventDefault();
            clearTimeout(picker.searchTimer);
            searchGifs(picker);
        } else if (ev.key === 'Escape') {
            // Back to the text box: an empty composer collapses its
            // buttons once focus leaves the form.
            const form = picker.closest('.comment-form');
            closeGifPicker(form);
            form.querySelector('.comment-body-input')?.focus();
        }
    });

    document.addEventListener('submit', function(ev) {
        const form = ev.target.closest('.comment-form');
        if (!form) return;

        ev.preventDefault();

        if (form.classList.contains('comment-edit-form')) {
            saveEdit(form);
            return;
        }

        postComment(form);
    });

    window.POLComments = { init };

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', () => init(document));
    } else {
        init(document);
    }
})();
