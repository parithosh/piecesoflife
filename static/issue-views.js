/**
 * Published issue pagination.
 *
 * Shows one question at a time, keeps the current question in ?q=N, and wires
 * progress segments, dot pager, bottom previous/next cards, and arrow keys.
 */

(function() {
    const root = document.getElementById('issue-root');
    if (!root) return;

    const questions = Array.from(root.querySelectorAll('.published-question'));
    if (!questions.length) return;

    const progressLinks = Array.from(root.querySelectorAll('.published-progress-segment'));
    const dotLinks = Array.from(root.querySelectorAll('.published-dot'));
    const positionLabel = root.querySelector('[data-question-position]');
    const compactLabel = root.querySelector('[data-question-position-compact]');
    const dotPager = root.querySelector('.published-dot-pager');
    const prevLink = root.querySelector('[data-pager-prev]');
    const nextLink = root.querySelector('[data-pager-next]');
    const prevTitle = root.querySelector('[data-pager-prev-title]');
    const nextTitle = root.querySelector('[data-pager-next-title]');
    const modeToggle = root.querySelector('[data-mode-toggle]');
    const modeButtons = Array.from(root.querySelectorAll('[data-read-mode]'));
    const unseenView = root.querySelector('[data-unseen-view]');
    const unseenBadge = root.querySelector('[data-unseen-badge]');
    const issueID = root.dataset.issueId || '';

    const MODE_KEY = 'pol-read-mode';
    let mode = 'paged';
    let lastReadingMode = 'paged';
    let markedSeen = false;
    try {
        if (localStorage.getItem(MODE_KEY) === 'scroll') {
            mode = 'scroll';
            lastReadingMode = 'scroll';
        }
    } catch {}

    let current = clamp(parseQuestionParam() - 1);

    function parseQuestionParam() {
        const q = Number(new URLSearchParams(window.location.search).get('q') || '1');
        return Number.isFinite(q) && q > 0 ? Math.floor(q) : 1;
    }

    function clamp(index) {
        return Math.max(0, Math.min(index, questions.length - 1));
    }

    function questionURL(index) {
        const url = new URL(window.location.href);
        url.searchParams.set('q', String(index + 1));
        return `${url.pathname}${url.search}${url.hash}`;
    }

    function titleFor(index) {
        return questions[index]?.dataset.questionTitle || '';
    }

    function shortTitle(text) {
        const clean = String(text || '').trim();
        return clean.length > 34 ? `${clean.slice(0, 31).trim()}...` : clean;
    }

    function markCommentsSeen() {
        if (markedSeen || !issueID) return;
        const unseenCount = Number(root.dataset.unseenCount || '0');
        if (unseenCount <= 0) return;
        markedSeen = true;
        if (unseenBadge) unseenBadge.remove();

        const headers = typeof apiHeaders === 'function'
            ? apiHeaders()
            : { 'Content-Type': 'application/json', 'X-CSRF-Token': typeof getCSRFToken === 'function' ? getCSRFToken() : '' };
        fetch(`/api/issues/${encodeURIComponent(issueID)}/comments/mark-seen`, {
            method: 'POST',
            headers,
        }).catch(() => {});
    }

    function applyMode(next) {
        if (next === 'unseen' && unseenView) {
            mode = 'unseen';
        } else {
            mode = next === 'scroll' ? 'scroll' : 'paged';
            lastReadingMode = mode;
        }

        root.classList.toggle('is-scroll', mode === 'scroll');
        root.classList.toggle('is-unseen', mode === 'unseen');
        if (unseenView) unseenView.hidden = mode !== 'unseen';

        modeButtons.forEach(btn => {
            const active = btn.dataset.readMode === mode;
            btn.classList.toggle('is-active', active);
            btn.setAttribute('aria-pressed', active ? 'true' : 'false');
        });

        if (mode === 'unseen') {
            markCommentsSeen();
            return;
        }

        if (mode === 'scroll') {
            questions.forEach(section => { section.hidden = false; });
        } else {
            setCurrent(current, { replace: true });
        }

        try { localStorage.setItem(MODE_KEY, mode); } catch {}
    }

    function scrollToQuestion(index) {
        const section = questions[clamp(index)];
        if (!section) return;
        section.scrollIntoView({ block: 'start' });
        section.querySelector('.published-question-title')?.focus?.();
    }

    function setCurrent(index, opts = {}) {
        current = clamp(index);

        if (mode !== 'scroll') {
            questions.forEach((section, i) => {
                section.hidden = i !== current;
            });
        }

        [...progressLinks, ...dotLinks].forEach(link => {
            const i = Number(link.dataset.questionTarget);
            link.classList.toggle('is-past', i < current);
            link.classList.toggle('is-current', i === current);
            link.classList.toggle('is-upcoming', i > current);
            if (i === current) {
                link.setAttribute('aria-current', 'step');
            } else {
                link.removeAttribute('aria-current');
            }
            link.href = questionURL(i);
        });

        const cur = questions[current];
        let posLabel = `Question ${current + 1} of ${questions.length}`;
        if (cur?.hasAttribute('data-diary-page')) {
            posLabel = `From the notebooks · ${current + 1} of ${questions.length}`;
        } else if (cur?.hasAttribute('data-peoples-choice-page')) {
            posLabel = `The hot seat · ${current + 1} of ${questions.length}`;
        } else if (cur?.hasAttribute('data-dump-page')) {
            posLabel = `The dump · ${current + 1} of ${questions.length}`;
        } else if (cur?.hasAttribute('data-throwback-page')) {
            posLabel = `Photos from one year ago · ${current + 1} of ${questions.length}`;
        }
        if (positionLabel) {
            positionLabel.textContent = posLabel;
        }
        if (compactLabel) {
            compactLabel.textContent = `${current + 1} / ${questions.length}`;
        }
        if (dotPager) {
            dotPager.dataset.currentLabel = posLabel;
        }

        updatePagerCard(prevLink, current - 1, prevTitle, 'Previous question', current > 0);
        updatePagerCard(nextLink, current + 1, nextTitle, 'Next question', current < questions.length - 1);

        if (nextLink && current === questions.length - 1) {
            nextLink.href = '/issues';
            nextLink.classList.remove('is-disabled');
            nextLink.classList.add('is-finish');
            nextLink.removeAttribute('aria-disabled');
            nextLink.setAttribute('aria-label', 'Back to all issues');
            nextLink.querySelector('.published-pager-kicker').textContent = 'Finish';
            if (nextTitle) nextTitle.textContent = 'Back to all issues';
        } else if (nextLink) {
            nextLink.classList.remove('is-finish');
            nextLink.querySelector('.published-pager-kicker').textContent = 'Next';
        }

        if (opts.push) {
            window.history.pushState({ q: current + 1 }, '', questionURL(current));
        } else if (opts.replace) {
            window.history.replaceState({ q: current + 1 }, '', questionURL(current));
        }

        if (opts.focus) {
            questions[current]?.querySelector('.published-question-title')?.focus?.();
        }
    }

    function updatePagerCard(link, target, titleEl, label, enabled) {
        if (!link) return;

        if (!enabled) {
            link.classList.add('is-disabled');
            link.setAttribute('aria-disabled', 'true');
            link.removeAttribute('href');
            if (titleEl) titleEl.textContent = '';
            return;
        }

        link.classList.remove('is-disabled');
        link.removeAttribute('aria-disabled');
        link.href = questionURL(target);
        link.setAttribute('aria-label', `${label}: ${titleFor(target)}`);
        if (titleEl) titleEl.textContent = shortTitle(titleFor(target));
    }

    function go(index) {
        setCurrent(index, { push: true });
        root.scrollIntoView({ block: 'start' });
    }

    root.addEventListener('click', ev => {
        const modeBtn = ev.target.closest('[data-read-mode]');
        if (modeBtn) {
            applyMode(modeBtn.dataset.readMode);
            return;
        }

        const jumpBtn = ev.target.closest('[data-jump-page]');
        if (jumpBtn) {
            const targetIdx = clamp(Number(jumpBtn.dataset.jumpPage));
            applyMode(lastReadingMode);
            if (mode === 'scroll') {
                scrollToQuestion(targetIdx);
            } else {
                go(targetIdx);
            }
            return;
        }

        const link = ev.target.closest('[data-question-target]');
        if (link) {
            ev.preventDefault();
            if (mode === 'scroll') {
                scrollToQuestion(Number(link.dataset.questionTarget));
            } else {
                go(Number(link.dataset.questionTarget));
            }
            return;
        }

        const pager = ev.target.closest('[data-pager-prev], [data-pager-next]');
        if (!pager || pager.getAttribute('aria-disabled') === 'true') {
            if (pager) ev.preventDefault();
            return;
        }

        if (pager.dataset.pagerPrev !== undefined) {
            ev.preventDefault();
            go(current - 1);
            return;
        }

        if (pager.dataset.pagerNext !== undefined && current < questions.length - 1) {
            ev.preventDefault();
            go(current + 1);
        }
    });

    window.addEventListener('popstate', () => {
        if (mode === 'scroll') {
            scrollToQuestion(parseQuestionParam() - 1);
            return;
        }
        setCurrent(clamp(parseQuestionParam() - 1), { focus: true });
    });

    document.addEventListener('keydown', ev => {
        if (mode !== 'paged') return;

        const active = document.activeElement;
        const editing = active && ['INPUT', 'TEXTAREA', 'SELECT'].includes(active.tagName);
        if (editing || ev.metaKey || ev.ctrlKey || ev.altKey) return;

        if (ev.key === 'ArrowLeft' && current > 0) {
            ev.preventDefault();
            go(current - 1);
        } else if (ev.key === 'ArrowRight' && current < questions.length - 1) {
            ev.preventDefault();
            go(current + 1);
        }
    });

    if (modeToggle) modeToggle.hidden = false;
    setCurrent(current, { replace: true });
    if (mode === 'scroll') {
        applyMode('scroll');
        if (parseQuestionParam() > 1) scrollToQuestion(parseQuestionParam() - 1);
    } else {
        applyMode('paged');
    }
})();
