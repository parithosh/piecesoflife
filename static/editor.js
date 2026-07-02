/**
 * PiecesOfLife Response Editor
 *
 * Provides autosave, block management, media upload, and link-paste
 * detection for the response editor page. Loaded from respond.html; the
 * page sets window.__pol.issueID and window.__pol.responseAPIBase before
 * this script runs.
 */

// Server-side upload cap (MaxBytesReader). Checked client-side too so a
// 700 MB pick fails instantly with a clear message instead of uploading
// for minutes and dying with a generic error.
const MAX_UPLOAD_BYTES = 200 * 1024 * 1024;

function uploadTooLarge(file) {
    if (file.size <= MAX_UPLOAD_BYTES) return null;
    const mb = Math.round(file.size / (1024 * 1024));
    return `${file.name || 'That file'} is ${mb} MB — uploads are capped at 200 MB. Try trimming or compressing it.`;
}

// Helper: get CSRF token from cookie.
function getCSRFToken() {
    return document.cookie
        .split('; ')
        .find(row => row.startsWith('csrf_token='))
        ?.split('=')[1] || '';
}

// Helper: standard API headers.
function apiHeaders() {
    return {
        'Content-Type': 'application/json',
        'X-CSRF-Token': getCSRFToken(),
    };
}

// Helper: show a toast notification.
function showToast(message, duration) {
    duration = duration || 3000;
    const toast = document.createElement('div');
    toast.className = 'toast';
    toast.textContent = message;
    document.body.appendChild(toast);
    requestAnimationFrame(function() {
        toast.classList.add('show');
    });
    setTimeout(function() {
        toast.classList.remove('show');
        setTimeout(function() { toast.remove(); }, 300);
    }, duration);
}

// Helper: upload a durable media block.
async function uploadMedia(responseID, file, kind) {
    const sizeErr = uploadTooLarge(file);
    if (sizeErr) throw new Error(sizeErr);

    const formData = new FormData();
    formData.append('kind', kind || 'photo');
    formData.append('media', file);

    const res = await fetch('/api/responses/' + responseID + '/blocks/upload', {
        method: 'POST',
        headers: { 'X-CSRF-Token': getCSRFToken() },
        body: formData,
    });

    if (!res.ok) {
        const data = await res.json();
        throw new Error(data.error?.message || 'Upload failed');
    }

    return res.json();
}

// Helper: upload a photo.
async function uploadPhoto(responseID, file) {
    return uploadMedia(responseID, file, 'photo');
}

// --- Link-block detection on paste -------------------------------------
//
// If the user pastes a Spotify / YouTube / Apple Music / SoundCloud URL
// into a text area, offer to convert it into a dedicated link block. We
// don't auto-replace (that's disruptive if the URL is incidental); we
// insert an inline "Convert to link block" chip next to the textarea.

const LINK_PATTERNS = [
    { name: 'YouTube',     re: /^https?:\/\/(?:www\.)?(?:youtube\.com\/watch\?v=|youtu\.be\/)[A-Za-z0-9_\-]+/ },
    { name: 'Spotify',     re: /^https?:\/\/open\.spotify\.com\/(track|album|playlist|episode|show)\/[A-Za-z0-9]+/ },
    { name: 'Apple Music', re: /^https?:\/\/music\.apple\.com\/[a-z]{2}\/(album|song|playlist)\/[^?\s]+/ },
    { name: 'SoundCloud',  re: /^https?:\/\/(?:www\.)?soundcloud\.com\/[A-Za-z0-9_\-\/]+/ },
];

function detectLinkKind(url) {
    for (const p of LINK_PATTERNS) {
        if (p.re.test(url)) return p.name;
    }
    return null;
}

function attachPasteDetectors() {
    document.querySelectorAll('.response-text').forEach(textarea => {
        if (textarea.dataset.linkHandlerAttached) return;
        textarea.dataset.linkHandlerAttached = '1';

        textarea.addEventListener('paste', function(ev) {
            const pasted = (ev.clipboardData || window.clipboardData)?.getData('text') || '';
            const trimmed = pasted.trim();
            const kind = detectLinkKind(trimmed);
            if (!kind) return;

            // Defer so the pasted text is already in the textarea.
            setTimeout(() => offerLinkConversion(textarea, trimmed, kind), 0);
        });
    });
}

function offerLinkConversion(textarea, url, kind) {
    // Avoid stacking chips if user pastes multiple times.
    const section = textarea.closest('.question-section');
    const existing = section.querySelector('.link-suggest');
    if (existing) existing.remove();

    const chip = document.createElement('button');
    chip.type = 'button';
    chip.className = 'link-suggest';
    chip.textContent = `Convert pasted ${kind} link into a link block`;
    chip.style.cssText = 'margin:8px 0;padding:8px 12px;background:var(--ivory-2,#fffaf0);border:1.5px dashed var(--zari,#c8962c);border-radius:4px;cursor:pointer;font-size:13px;font-family:var(--font-ui,sans-serif);font-weight:600;color:var(--rani,#7a0f38);';

    chip.addEventListener('click', async function() {
        chip.disabled = true;
        chip.textContent = 'Converting…';

        const responseID = section.querySelector('[data-response-id]')?.dataset.responseId;
        if (!responseID) {
            // No response yet — trigger autosave first to create one.
            await autosave(section);
            const again = section.querySelector('[data-response-id]')?.dataset.responseId;
            if (!again) {
                chip.textContent = 'Could not create response';
                return;
            }
            await createLinkBlock(again, url);
        } else {
            await createLinkBlock(responseID, url);
        }

        // Remove the URL from the textarea and refresh the page to show
        // the new block in its server-rendered form.
        textarea.value = textarea.value.replace(url, '').trim();
        chip.remove();
        showToast('Link block added — reloading');
        setTimeout(() => window.location.reload(), 600);
    });

    textarea.insertAdjacentElement('afterend', chip);
}

async function createLinkBlock(responseID, url) {
    return fetch(`/api/responses/${responseID}/blocks`, {
        method: 'POST',
        headers: apiHeaders(),
        body: JSON.stringify({
            type: 'link',
            link_url: url,
            sort_order: 9999,
        }),
    });
}

// --- Drag-to-reorder ---------------------------------------------------
//
// Each .blocks container becomes a Sortable list. On drop, gather the new
// data-block-id order and POST it to /reorder. New (unpersisted) text blocks
// without an id are skipped from the payload — they get sort orders the next
// time autosave runs.

async function persistReorder(blocksEl) {
    const responseID = blocksEl.closest('[data-response-id]')?.dataset.responseId;
    if (!responseID) return;

    const orderedIDs = [];
    blocksEl.querySelectorAll(':scope > .block').forEach(b => {
        const id = b.dataset.blockId;
        if (id && id !== '0') orderedIDs.push(parseInt(id, 10));
    });

    if (orderedIDs.length < 2) return;

    try {
        const res = await fetch(`/api/responses/${responseID}/blocks/reorder`, {
            method: 'POST',
            headers: apiHeaders(),
            body: JSON.stringify({ ordered_ids: orderedIDs }),
        });
        if (!res.ok) showToast('Could not save order — please retry');
    } catch {
        showToast('Could not save order — please retry');
    }
}

function attachSortable() {
    if (typeof Sortable === 'undefined') return;

    document.querySelectorAll('.blocks').forEach(container => {
        if (container.dataset.sortableAttached) return;
        container.dataset.sortableAttached = '1';

        Sortable.create(container, {
            animation: 150,
            handle: '.drag-handle',
            ghostClass: 'block-ghost',
            onEnd: () => persistReorder(container),
        });
    });
}

// --- Photo & video dump ---------------------------------------------------
//
// The dump is issue-level media (not tied to a question) collected before
// submit and rendered as the collage closer of the published issue.

function dumpThumb(item, url) {
    const fig = document.createElement('figure');
    fig.className = 'pl-dump-thumb';
    fig.dataset.dumpId = item.id;

    if (item.kind === 'video') {
        const video = document.createElement('video');
        video.src = url;
        video.preload = 'metadata';
        video.muted = true;
        video.playsInline = true;
        fig.appendChild(video);

        const kind = document.createElement('span');
        kind.className = 'pl-dump-thumb-kind';
        kind.setAttribute('aria-hidden', 'true');
        kind.textContent = '▶';
        fig.appendChild(kind);
    } else {
        const img = document.createElement('img');
        img.src = url;
        img.alt = '';
        fig.appendChild(img);
    }

    const remove = document.createElement('button');
    remove.type = 'button';
    remove.className = 'pl-dump-remove';
    remove.dataset.dumpRemove = item.id;
    remove.setAttribute('aria-label', 'Remove from dump');
    remove.textContent = '✕';
    fig.appendChild(remove);

    return fig;
}

function attachDump() {
    const grid = document.getElementById('dump-grid');
    if (!grid) return;

    const issueID = grid.dataset.issueId;
    const status = document.getElementById('dump-status');

    const setStatus = (msg, isError) => {
        if (!status) return;
        status.textContent = msg || '';
        status.classList.toggle('is-error', Boolean(isError));
    };

    async function uploadDump(file, kind) {
        const sizeErr = uploadTooLarge(file);
        if (sizeErr) throw new Error(sizeErr);

        const formData = new FormData();
        formData.append('kind', kind);
        formData.append('media', file);

        const res = await fetch(`/api/issues/${issueID}/dump`, {
            method: 'POST',
            headers: { 'X-CSRF-Token': getCSRFToken() },
            body: formData,
        });

        const data = await res.json().catch(() => ({}));
        if (!res.ok) {
            throw new Error(data.error?.message || 'Upload failed');
        }

        grid.appendChild(dumpThumb(data.item, data.url));
        grid.classList.remove('is-empty');
    }

    function wireInput(btnID, inputID, kind) {
        const btn = document.getElementById(btnID);
        const input = document.getElementById(inputID);
        if (!btn || !input) return;

        btn.addEventListener('click', () => input.click());

        input.addEventListener('change', async function() {
            const files = Array.from(input.files || []);
            input.value = '';
            if (!files.length) return;

            btn.disabled = true;
            for (let i = 0; i < files.length; i++) {
                setStatus(files.length > 1
                    ? `Uploading ${i + 1} of ${files.length}…`
                    : 'Uploading…');
                try {
                    await uploadDump(files[i], kind);
                } catch (err) {
                    setStatus(err.message, true);
                    btn.disabled = false;
                    return;
                }
            }
            setStatus('Added to the dump ✓');
            btn.disabled = false;
        });
    }

    wireInput('dump-photo-btn', 'dump-photo-input', 'photo');
    wireInput('dump-video-btn', 'dump-video-input', 'video');

    grid.addEventListener('click', async ev => {
        const btn = ev.target.closest('[data-dump-remove]');
        if (!btn) return;

        btn.disabled = true;
        try {
            const res = await fetch(`/api/dump/${btn.dataset.dumpRemove}`, {
                method: 'DELETE',
                headers: { 'X-CSRF-Token': getCSRFToken() },
            });
            if (!res.ok) {
                const data = await res.json().catch(() => ({}));
                throw new Error(data.error?.message || 'Remove failed');
            }
            btn.closest('.pl-dump-thumb')?.remove();
            if (!grid.querySelector('.pl-dump-thumb')) grid.classList.add('is-empty');
            setStatus('Removed');
        } catch (err) {
            setStatus(err.message, true);
            btn.disabled = false;
        }
    });
}

// Expose for respond.html to call after rendering.
window.__polEditor = {
    attachPasteDetectors,
    attachSortable,
    getCSRFToken,
    apiHeaders,
    uploadMedia,
    uploadPhoto,
    createLinkBlock,
    showToast,
};

// Run once on load.
function initEditor() {
    attachPasteDetectors();
    attachSortable();
    attachDump();
}

if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', initEditor);
} else {
    initEditor();
}
