/**
 * Lightweight image lightbox.
 *
 * Any element with data-lightbox-src (or a descendant <img> with src) becomes
 * clickable. On click it overlays the full-size image with keyboard nav:
 *   Esc  — close
 *   ←/→  — previous / next (among all lightbox-enabled images on the page)
 *
 * Touch: swipe left/right to navigate, a generous vertical swipe closes.
 * Focus is trapped inside the overlay while open and restored on close.
 *
 * No dependencies, no styles needed elsewhere — the overlay is inline-styled
 * so it works even if app.css doesn't load.
 */

(function() {
    let overlay = null;
    let currentIndex = 0;
    let images = [];
    let lastFocused = null;

    // Touch swipe tracking.
    const SWIPE_X_THRESHOLD = 50;  // px of horizontal travel → prev/next
    const SWIPE_Y_THRESHOLD = 80;  // px of vertical travel → close
    let touchActive = false;
    let touchStartX = 0;
    let touchStartY = 0;

    function prefersReducedMotion() {
        return window.matchMedia &&
            window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    }

    function collectImages() {
        images = Array.from(document.querySelectorAll('[data-lightbox]'))
            .map(el => {
                const img = el.tagName === 'IMG' ? el : el.querySelector('img');
                return {
                    src: el.dataset.lightboxSrc || img?.src || '',
                    caption: el.dataset.lightboxCaption || img?.alt || '',
                };
            })
            .filter(x => x.src);
    }

    function ensureOverlay() {
        if (overlay) return;

        overlay = document.createElement('div');
        overlay.id = 'lightbox-overlay';
        overlay.setAttribute('role', 'dialog');
        overlay.setAttribute('aria-modal', 'true');
        overlay.setAttribute('aria-label', 'Image viewer');
        overlay.style.cssText = [
            'position:fixed', 'inset:0', 'background:rgba(0,0,0,0.9)',
            'display:none', 'align-items:center', 'justify-content:center',
            'z-index:9999', 'cursor:zoom-out', 'touch-action:none',
        ].join(';');

        overlay.innerHTML = `
            <img id="lightbox-img" style="max-width:94vw;max-height:86vh;object-fit:contain;border-radius:6px;box-shadow:0 20px 60px rgba(0,0,0,0.5);">
            <div id="lightbox-caption" style="position:absolute;bottom:24px;left:0;right:0;text-align:center;color:#eee;font-size:14px;padding:0 16px;"></div>
            <button id="lightbox-prev" type="button" aria-label="Previous image" style="position:absolute;left:16px;top:50%;transform:translateY(-50%);background:rgba(255,255,255,0.1);color:#fff;border:none;width:44px;height:44px;border-radius:50%;font-size:20px;cursor:pointer;">‹</button>
            <button id="lightbox-next" type="button" aria-label="Next image" style="position:absolute;right:16px;top:50%;transform:translateY(-50%);background:rgba(255,255,255,0.1);color:#fff;border:none;width:44px;height:44px;border-radius:50%;font-size:20px;cursor:pointer;">›</button>
            <button id="lightbox-close" type="button" aria-label="Close image viewer" style="position:absolute;top:16px;right:16px;background:rgba(255,255,255,0.1);color:#fff;border:none;width:40px;height:40px;border-radius:50%;font-size:22px;cursor:pointer;">×</button>
        `;

        document.body.appendChild(overlay);

        overlay.addEventListener('click', function(ev) {
            if (ev.target === overlay) close();
        });
        overlay.querySelector('#lightbox-close').addEventListener('click', close);
        overlay.querySelector('#lightbox-prev').addEventListener('click', prev);
        overlay.querySelector('#lightbox-next').addEventListener('click', next);

        // Touch swipe: horizontal → prev/next, generous vertical → close.
        // Only single-finger gestures; taps below the thresholds fall through
        // to normal clicks so the buttons and click-to-close keep working.
        overlay.addEventListener('touchstart', function(ev) {
            if (ev.touches.length !== 1) {
                touchActive = false;
                return;
            }
            touchActive = true;
            touchStartX = ev.touches[0].clientX;
            touchStartY = ev.touches[0].clientY;
        }, { passive: true });

        overlay.addEventListener('touchmove', function(ev) {
            if (!touchActive || ev.touches.length !== 1) return;
            const dx = ev.touches[0].clientX - touchStartX;
            const dy = ev.touches[0].clientY - touchStartY;
            // Once the finger has clearly moved, stop iOS overscroll /
            // pull-to-refresh from stealing the gesture. The page behind is
            // already scroll-locked, so nothing legitimate is blocked.
            if (Math.abs(dx) > 10 || Math.abs(dy) > 10) ev.preventDefault();
        }, { passive: false });

        overlay.addEventListener('touchend', function(ev) {
            if (!touchActive) return;
            touchActive = false;
            const t = ev.changedTouches[0];
            const dx = t.clientX - touchStartX;
            const dy = t.clientY - touchStartY;
            if (Math.abs(dx) >= SWIPE_X_THRESHOLD && Math.abs(dx) > Math.abs(dy)) {
                // Dominant-axis check: horizontal wins → navigate.
                ev.preventDefault(); // suppress the synthetic click
                if (dx < 0) next(); else prev();
            } else if (Math.abs(dy) >= SWIPE_Y_THRESHOLD && Math.abs(dy) > Math.abs(dx)) {
                // Generous vertical swipe (down or up) → close.
                ev.preventDefault();
                close();
            }
        }, { passive: false });

        overlay.addEventListener('touchcancel', function() {
            touchActive = false;
        }, { passive: true });
    }

    function focusableElements() {
        return Array.from(overlay.querySelectorAll('button'))
            .filter(el => el.style.visibility !== 'hidden' && !el.disabled);
    }

    function open(index) {
        if (!images.length) return;
        ensureOverlay();
        currentIndex = Math.max(0, Math.min(index, images.length - 1));
        render();
        lastFocused = document.activeElement;
        overlay.style.display = 'flex';
        if (!prefersReducedMotion()) {
            overlay.style.opacity = '0';
            requestAnimationFrame(function() {
                overlay.style.transition = 'opacity 0.18s ease';
                overlay.style.opacity = '1';
            });
        } else {
            overlay.style.transition = '';
            overlay.style.opacity = '1';
        }
        document.body.style.overflow = 'hidden';
        overlay.querySelector('#lightbox-close').focus();
    }

    function close() {
        if (!overlay) return;
        overlay.style.display = 'none';
        overlay.style.transition = '';
        overlay.style.opacity = '';
        document.body.style.overflow = '';
        touchActive = false;
        // Restore focus to whatever opened the lightbox.
        if (lastFocused && typeof lastFocused.focus === 'function' &&
            document.contains(lastFocused)) {
            lastFocused.focus();
        }
        lastFocused = null;
    }

    function render() {
        const img = overlay.querySelector('#lightbox-img');
        const caption = overlay.querySelector('#lightbox-caption');
        const cur = images[currentIndex];
        img.src = cur.src;
        img.alt = cur.caption || '';
        caption.textContent = cur.caption || '';

        // Name the dialog after the image so screen readers announce it.
        overlay.setAttribute('aria-label', cur.caption || 'Image viewer');

        overlay.querySelector('#lightbox-prev').style.visibility =
            images.length > 1 ? 'visible' : 'hidden';
        overlay.querySelector('#lightbox-next').style.visibility =
            images.length > 1 ? 'visible' : 'hidden';
    }

    function prev() {
        if (!images.length) return;
        currentIndex = (currentIndex - 1 + images.length) % images.length;
        render();
    }

    function next() {
        if (!images.length) return;
        currentIndex = (currentIndex + 1) % images.length;
        render();
    }

    document.addEventListener('keydown', function(ev) {
        if (!overlay || overlay.style.display !== 'flex') return;
        switch (ev.key) {
            case 'Escape': close(); break;
            case 'ArrowLeft': prev(); break;
            case 'ArrowRight': next(); break;
            case 'Tab': {
                // Trap focus inside the overlay while it is open.
                const focusables = focusableElements();
                if (!focusables.length) {
                    ev.preventDefault();
                    break;
                }
                const idx = focusables.indexOf(document.activeElement);
                let nextIdx;
                if (ev.shiftKey) {
                    nextIdx = idx <= 0 ? focusables.length - 1 : idx - 1;
                } else {
                    nextIdx = (idx === -1 || idx === focusables.length - 1) ? 0 : idx + 1;
                }
                ev.preventDefault();
                focusables[nextIdx].focus();
                break;
            }
        }
    });

    function attach() {
        collectImages();

        document.querySelectorAll('[data-lightbox]').forEach((el, idx) => {
            if (el.dataset.lightboxWired) return;
            el.dataset.lightboxWired = '1';
            el.style.cursor = 'zoom-in';
            el.addEventListener('click', function(ev) {
                ev.preventDefault();
                collectImages(); // refresh in case DOM changed
                open(images.findIndex(i => i.src === (el.dataset.lightboxSrc || el.querySelector('img')?.src)));
            });
        });
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', attach);
    } else {
        attach();
    }

    // Expose for pages that inject photos dynamically.
    window.__polLightbox = { attach, open, close };
})();
