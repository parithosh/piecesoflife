/**
 * Collapsed comments widget for the published issue view.
 */

(function() {
    const ALLOWED_COMMENT_TAGS = new Set([
        'p', 'br', 'strong', 'em', 'code', 'pre', 'blockquote',
        'ul', 'ol', 'li', 'a',
    ]);

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
                        <button type="submit" disabled>Post</button>
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
            const responseID = panel.dataset.responseId;
            const res = await fetch(`/api/responses/${responseID}/comments`);
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
        return `
            <li class="comment-item" data-comment-id="${c.id}">
                <div class="comment-meta">
                    <strong>${escapeHtml(c.author_name)}</strong>
                    <span>${formatTimestamp(c.created_at)}</span>
                </div>
                <div class="comment-body">${safeCommentHTML(c)}</div>
                <button type="button" class="comment-reply-btn"
                        data-reply-to="${replyTarget}"
                        aria-label="Reply to ${escapeHtml(c.author_name)}">↳ reply</button>
                ${replies && replies.length ? `
                <ul class="comment-replies">
                    ${replies.map(r => commentItemHTML(r, null)).join('')}
                </ul>` : ''}
            </li>
        `;
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

        // One reply composer at a time; clicking reply again closes it.
        const existing = item.querySelector(':scope > .comment-reply-form');
        removeReplyForm(document);
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
        const panel = form.closest('.comments-panel[data-response-id]');
        if (!panel) return;

        const responseID = panel.dataset.responseId;
        const textarea = form.querySelector('.comment-body-input');
        const status = form.querySelector('.comment-status');
        const submit = form.querySelector('button[type="submit"]');
        const body = textarea.value.trim();
        if (!body) return;

        status.textContent = 'Posting...';
        status.dataset.state = 'pending';
        submit.disabled = true;

        const payload = { body };
        const parentID = Number(form.dataset.parentId || 0);
        if (parentID > 0) payload.parent_id = parentID;

        try {
            const res = await fetch(`/api/responses/${responseID}/comments`, {
                method: 'POST',
                headers: apiHeaders(),
                body: JSON.stringify(payload),
            });

            if (res.ok) {
                textarea.value = '';
                form.classList.remove('has-content');
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
            submit.disabled = textarea.value.trim().length === 0;
        }
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
            return d.toLocaleDateString();
        } catch {
            return iso;
        }
    }

    function init(root) {
        const scope = root || document;
        scope.querySelectorAll('.comments-panel[data-response-id]').forEach(setupPanel);
    }

    document.addEventListener('click', ev => {
        const replyBtn = ev.target.closest('.comment-reply-btn');
        if (replyBtn) {
            const panel = replyBtn.closest('.comments-panel[data-response-id]');
            if (panel) openReplyForm(panel, replyBtn);
            return;
        }

        if (ev.target.closest('.comment-reply-cancel')) {
            removeReplyForm(document);
            return;
        }

        const btn = ev.target.closest('.comment-toggle');
        if (!btn) return;

        const panel = btn.closest('.comments-panel[data-response-id]');
        if (panel) togglePanel(panel);
    });

    document.addEventListener('input', ev => {
        const textarea = ev.target.closest('.comment-body-input');
        if (!textarea) return;

        const form = textarea.closest('.comment-form');
        const hasContent = textarea.value.trim().length > 0;
        form?.classList.toggle('has-content', hasContent);
        const submit = form?.querySelector('button[type="submit"]');
        if (submit) submit.disabled = !hasContent;
    });

    document.addEventListener('submit', function(ev) {
        const form = ev.target.closest('.comment-form');
        if (!form) return;

        ev.preventDefault();
        postComment(form);
    });

    window.POLComments = { init };

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', () => init(document));
    } else {
        init(document);
    }
})();
