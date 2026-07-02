/**
 * PiecesOfLife service worker.
 *
 * Strategy:
 *   - /static/*  → cache-first with versioned cache name, network fallback
 *                  and background revalidate.
 *   - HTML, API  → network-first, fall back to cache only for navigations
 *                  when offline (so the app shell appears rather than a
 *                  browser error). Never caches API JSON — that stays
 *                  source-of-truth only.
 *
 * Keep this file tiny and dependency-free. Intentionally no precache
 * list: the app works fully server-rendered, so caching on first use
 * is enough and avoids coordinating an asset manifest.
 */

// The registration URL carries ?v=<asset version> (see base.html). A new
// deploy changes the URL, which installs a fresh worker whose cache name
// differs — activate() then drops the stale cache.
const VERSION = new URL(self.location.href).searchParams.get('v') || 'dev';
const CACHE = 'pol-static-' + VERSION;
const STATIC_PREFIX = '/static/';

self.addEventListener('install', function(event) {
    self.skipWaiting();
});

self.addEventListener('activate', function(event) {
    event.waitUntil(
        caches.keys().then(function(keys) {
            return Promise.all(
                keys.filter(function(k) { return k !== CACHE; })
                    .map(function(k) { return caches.delete(k); })
            );
        }).then(function() { return self.clients.claim(); })
    );
});

self.addEventListener('fetch', function(event) {
    const req = event.request;
    if (req.method !== 'GET') return;

    const url = new URL(req.url);
    if (url.origin !== self.location.origin) return;

    if (url.pathname.startsWith(STATIC_PREFIX)) {
        event.respondWith(cacheFirst(req));
        return;
    }

    // For navigations, fall back to cached index-style pages offline.
    if (req.mode === 'navigate') {
        event.respondWith(networkWithCachedShell(req));
        return;
    }

    // Everything else (API, uploads) — straight through.
});

async function cacheFirst(req) {
    const cache = await caches.open(CACHE);
    const cached = await cache.match(req);

    if (cached) {
        // Kick off a background revalidate so the user gets updated assets
        // on next load without blocking this request.
        fetch(req).then(function(res) {
            if (res && res.ok) cache.put(req, res.clone());
        }).catch(function() {});
        return cached;
    }

    try {
        const res = await fetch(req);
        if (res && res.ok) cache.put(req, res.clone());
        return res;
    } catch (e) {
        return new Response('', { status: 504, statusText: 'Offline' });
    }
}

async function networkWithCachedShell(req) {
    try {
        const res = await fetch(req);
        return res;
    } catch (e) {
        const cache = await caches.open(CACHE);
        const cached = await cache.match('/');
        return cached || offlineResponse();
    }
}

// Minimal Pallu-styled offline page for navigations with nothing cached.
function offlineResponse() {
    const html = '<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">' +
        '<meta name="viewport" content="width=device-width, initial-scale=1">' +
        '<title>Offline — PiecesOfLife</title></head>' +
        '<body style="margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;' +
        'background:#efe4cb;font-family:Georgia,serif;color:#2a0e15;">' +
        '<div style="max-width:380px;margin:24px;padding:36px 32px;background:#fbf4e3;' +
        'border:1.5px solid #7a0f38;border-radius:5px;box-shadow:10px 10px 0 rgba(122,15,56,.18);text-align:center;">' +
        '<div style="font-size:34px;color:#c3362b;">✽</div>' +
        '<h1 style="margin:10px 0 8px;font-weight:400;font-size:28px;color:#7a0f38;">You&#39;re offline</h1>' +
        '<p style="margin:0;font-size:16px;line-height:1.6;color:#8a6d55;">' +
        'PiecesOfLife needs a connection to fetch this page. ' +
        'Check your internet and try again.</p>' +
        '<button onclick="location.reload()" style="margin-top:20px;padding:11px 22px;border:0;border-radius:4px;' +
        'background:#7a0f38;color:#fbf4e3;font-family:system-ui,sans-serif;font-weight:700;font-size:12px;' +
        'letter-spacing:.05em;text-transform:uppercase;cursor:pointer;">Try again</button>' +
        '</div></body></html>';
    return new Response(html, {
        status: 503,
        headers: { 'Content-Type': 'text/html; charset=utf-8' },
    });
}
