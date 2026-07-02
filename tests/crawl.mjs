// Screenshot crawler for the PiecesOfLife UI.
//
// Walks every interesting route at three viewport widths, captures
// full-page PNGs, and writes an index.html that lays them out side by
// side for fast visual review. Also collects console errors and failed
// network requests per page so rendering bugs surface as text, not just
// pixels.
//
// Run: npm install && npm run snap (then open screenshots/index.html)
//
// Assumes the app is up at http://localhost:8080 with DEV_MODE=true so
// /dev/login works.

import { chromium } from 'playwright';
import { mkdir, writeFile, rm } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const OUT_DIR = path.join(__dirname, 'screenshots');
const BASE = process.env.BASE_URL || 'http://localhost:8090';
const ADMIN_EMAIL = process.env.ADMIN_EMAIL || 'admin@example.com';

// Routes to capture. Some pages need an already-logged-in admin
// session; others (login) should be hit logged-out.
const ROUTES = [
    { name: '01-login',         path: '/login',           auth: false },
    { name: '02-landing',       path: '/',                auth: true  },
    { name: '03-admin',         path: '/admin',           auth: true  },
    { name: '04-admin-members', path: '/admin/members',   auth: true  },
    { name: '05-admin-questions', path: '/admin/questions', auth: true },
    { name: '06-admin-settings', path: '/admin/settings', auth: true  },
    { name: '07-archive',       path: '/issues',          auth: true  },
    { name: '08-profile',       path: '/profile',         auth: true  },
    { name: '09-albums',        path: '/albums',          auth: true  },
];

// Setup wizard only renders before setup completes — so it's listed
// here but skipped silently if the route 303s away. The crawler logs
// it under "skipped" rather than failing.
ROUTES.push({ name: '10-admin-setup', path: '/admin/setup', auth: true, optional: true });

// Wizard steps 2-4: same URL, but click "Next" before screenshot.
// `clickNext` is the count of times to advance.
ROUTES.push({ name: '11-admin-setup-step2', path: '/admin/setup', auth: true, optional: true, clickNext: 1 });
ROUTES.push({ name: '12-admin-setup-step3', path: '/admin/setup', auth: true, optional: true, clickNext: 2, beforeShot: 'fillName' });
ROUTES.push({ name: '13-admin-setup-step4', path: '/admin/setup', auth: true, optional: true, clickNext: 3, beforeShot: 'fillName' });

const VIEWPORTS = [
    { name: 'desktop', width: 1280, height: 800 },
    { name: 'tablet',  width: 768,  height: 1024 },
    { name: 'mobile',  width: 390,  height: 844 },
];

async function authedContext(browser) {
    const ctx = await browser.newContext({ viewport: VIEWPORTS[0] });
    const page = await ctx.newPage();
    const url = `${BASE}/dev/login?email=${encodeURIComponent(ADMIN_EMAIL)}&redirect=/`;
    const res = await page.goto(url, { waitUntil: 'networkidle' });
    if (!res || !res.ok()) {
        throw new Error(`/dev/login failed (status ${res?.status()}). Is DEV_MODE=true?`);
    }
    await page.close();
    return ctx;
}

async function publicContext(browser) {
    return browser.newContext({ viewport: VIEWPORTS[0] });
}

function fmtIssue(i) {
    const status = i.status ? ` [${i.status}]` : '';
    return `- ${i.kind}${status}: ${i.detail}`;
}

async function snapRoute(ctx, route, viewport) {
    const page = await ctx.newPage();
    await page.setViewportSize({ width: viewport.width, height: viewport.height });

    const issues = [];

    page.on('console', msg => {
        if (msg.type() === 'error') {
            issues.push({ kind: 'console-error', detail: msg.text() });
        }
    });

    page.on('pageerror', err => {
        issues.push({ kind: 'page-error', detail: err.message });
    });

    page.on('requestfailed', req => {
        // Service worker registration failures spam the log on every
        // request; skip them.
        if (req.url().includes('/static/sw.js')) return;
        issues.push({
            kind: 'request-failed',
            detail: `${req.method()} ${req.url()} (${req.failure()?.errorText})`,
        });
    });

    page.on('response', res => {
        if (res.status() >= 400 && res.url().startsWith(BASE)) {
            issues.push({
                kind: 'http-error',
                status: res.status(),
                detail: `${res.request().method()} ${res.url()}`,
            });
        }
    });

    let finalURL = '';
    let status = 0;

    try {
        const res = await page.goto(`${BASE}${route.path}`, {
            waitUntil: 'networkidle',
            timeout: 15000,
        });
        finalURL = page.url();
        status = res?.status() ?? 0;
    } catch (err) {
        issues.push({ kind: 'goto-failed', detail: err.message });
    }

    // Brief settle for fonts and any JS-driven layout (countdown, etc.)
    await page.waitForTimeout(250);

    // Wizard step traversal: fill required fields per step, then click
    // Next the requested number of times.
    if (route.clickNext) {
        try {
            if (route.beforeShot === 'fillName') {
                const nameInput = page.locator('#admin-name');
                if (await nameInput.count()) await nameInput.fill('Test Admin');
            }
            for (let i = 0; i < route.clickNext; i++) {
                const next = page.locator('#wizard-next');
                if (!(await next.count()) || !(await next.isVisible())) break;
                await next.click();
                await page.waitForTimeout(150);
            }
        } catch (err) {
            issues.push({ kind: 'step-failed', detail: err.message });
        }
    }

    const file = `${route.name}.${viewport.name}.png`;
    const filePath = path.join(OUT_DIR, file);

    try {
        await page.screenshot({ path: filePath, fullPage: true });
    } catch (err) {
        issues.push({ kind: 'screenshot-failed', detail: err.message });
    }

    await page.close();

    const redirected = finalURL && finalURL !== `${BASE}${route.path}`;
    const skipped = route.optional && redirected;

    return { route, viewport, file, finalURL, status, issues, skipped };
}

async function main() {
    if (existsSync(OUT_DIR)) await rm(OUT_DIR, { recursive: true });
    await mkdir(OUT_DIR, { recursive: true });

    const browser = await chromium.launch();

    let authCtx, publicCtx;

    try {
        authCtx = await authedContext(browser);
    } catch (err) {
        console.error('Auth failed:', err.message);
        await browser.close();
        process.exit(1);
    }

    publicCtx = await publicContext(browser);

    // Discover the current respond + published-issue URLs from the archive
    // page — their paths depend on seeded data (issue IDs, month slugs).
    try {
        const page = await authCtx.newPage();
        await page.goto(`${BASE}/issues`, { waitUntil: 'networkidle' });
        const found = await page.evaluate(() => {
            const links = [...document.querySelectorAll('a[href^="/issues/"]')].map(a => a.getAttribute('href'));
            return {
                respond: links.find(h => h.endsWith('/respond')) || null,
                published: links.find(h => /^\/issues\/\d{4}\/\d{2}$/.test(h)) || null,
            };
        });
        await page.close();
        if (found.respond) ROUTES.push({ name: '14-respond', path: found.respond, auth: true });
        if (found.published) ROUTES.push({ name: '15-published', path: found.published, auth: true });
        if (!found.respond || !found.published) {
            console.warn('Discovery: missing', found.respond ? '' : 'respond', found.published ? '' : 'published');
        }
    } catch (err) {
        console.warn('Route discovery failed:', err.message);
    }

    const results = [];

    for (const route of ROUTES) {
        const ctx = route.auth ? authCtx : publicCtx;
        for (const vp of VIEWPORTS) {
            const r = await snapRoute(ctx, route, vp);
            results.push(r);
            const tag = r.skipped ? 'SKIP'
                      : r.issues.length === 0 ? 'OK  '
                      : `${r.issues.length} issue(s)`;
            console.log(`[${tag}] ${route.name} (${vp.name}) -> ${r.status} ${r.finalURL}`);
            for (const i of r.issues) console.log(`         ${fmtIssue(i)}`);
        }
    }

    await authCtx.close();
    await publicCtx.close();
    await browser.close();

    await writeIndex(results);

    const total   = results.length;
    const skipped = results.filter(r => r.skipped).length;
    const failed  = results.filter(r => !r.skipped && r.issues.length > 0).length;
    console.log(`\n${total - failed - skipped} clean, ${failed} with issues, ${skipped} skipped.`);
    console.log(`Open: file://${path.join(OUT_DIR, 'index.html')}`);
}

async function writeIndex(results) {
    // Group by route. Each row shows the three viewports side by side.
    const byRoute = new Map();
    for (const r of results) {
        if (!byRoute.has(r.route.name)) byRoute.set(r.route.name, []);
        byRoute.get(r.route.name).push(r);
    }

    const css = `
        body { font: 14px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
               background: #fafafa; color: #1f2937; padding: 24px; max-width: 1600px; margin: 0 auto; }
        h1 { margin: 0 0 8px; font-size: 22px; }
        .summary { color: #6b7280; margin-bottom: 24px; }
        .route { background: #fff; border: 1px solid #e5e7eb; border-radius: 12px;
                 padding: 16px; margin-bottom: 24px; }
        .route h2 { margin: 0 0 4px; font-size: 16px; display: flex; align-items: center; gap: 8px; }
        .route .url { color: #6b7280; font-size: 13px; font-family: ui-monospace, monospace; }
        .views { display: grid; grid-template-columns: 5fr 3fr 2fr; gap: 12px; margin-top: 12px; }
        .view { background: #f4f4f5; border-radius: 8px; padding: 8px; }
        .view header { display: flex; justify-content: space-between; font-size: 12px;
                       color: #6b7280; margin-bottom: 6px; }
        .view img { width: 100%; max-height: 800px; object-fit: contain; object-position: top;
                    background: #fff; border-radius: 4px; display: block; }
        .issues { margin-top: 8px; }
        .issue { background: #fef2f2; color: #991b1b; border: 1px solid #fecaca;
                 border-radius: 4px; padding: 6px 8px; margin: 4px 0; font-size: 12px;
                 font-family: ui-monospace, monospace; word-break: break-all; }
        .skip { color: #92400e; font-size: 12px; background: #fef9c3; padding: 4px 8px;
                border-radius: 4px; display: inline-block; }
        .ok { background: #f0fdf4; color: #166534; padding: 2px 8px; border-radius: 4px;
              font-size: 12px; }
        .bad { background: #fef2f2; color: #991b1b; padding: 2px 8px; border-radius: 4px;
               font-size: 12px; }
    `;

    const total = results.length;
    const skipped = results.filter(r => r.skipped).length;
    const failed = results.filter(r => !r.skipped && r.issues.length > 0).length;

    const sections = [...byRoute.entries()].map(([name, rs]) => {
        const skipped = rs.every(r => r.skipped);
        const issueCount = rs.reduce((s, r) => s + r.issues.length, 0);
        const status = skipped ? `<span class="skip">redirected — skipped</span>`
                     : issueCount ? `<span class="bad">${issueCount} issue(s)</span>`
                     : `<span class="ok">clean</span>`;

        const views = rs.map(r => `
            <div class="view">
                <header>
                    <span><strong>${r.viewport.name}</strong> ${r.viewport.width}×${r.viewport.height}</span>
                    <span>${r.status || '—'}</span>
                </header>
                ${r.skipped ? '<div class="skip">redirected to ' + escapeHtml(r.finalURL) + '</div>'
                            : `<img src="${r.file}" loading="lazy" alt="${r.route.name} ${r.viewport.name}">`}
                ${r.issues.length ? '<div class="issues">' +
                    r.issues.map(i => `<div class="issue">${escapeHtml(fmtIssue(i))}</div>`).join('') +
                    '</div>' : ''}
            </div>
        `).join('');

        const route = rs[0].route;
        return `
            <section class="route">
                <h2>${route.name} ${status}</h2>
                <div class="url">${route.path}${rs[0].finalURL && rs[0].finalURL !== BASE + route.path ? ' → ' + escapeHtml(rs[0].finalURL) : ''}</div>
                <div class="views">${views}</div>
            </section>
        `;
    }).join('');

    const html = `<!doctype html>
<html><head><meta charset="utf-8"><title>UI snapshots</title><style>${css}</style></head>
<body>
    <h1>PiecesOfLife UI snapshots</h1>
    <div class="summary">
        ${total - failed - skipped} clean, ${failed} with issues, ${skipped} skipped — ${new Date().toLocaleString()}
    </div>
    ${sections}
</body></html>`;

    await writeFile(path.join(OUT_DIR, 'index.html'), html);
}

function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, c => ({
        '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
    }[c]));
}

main().catch(err => { console.error(err); process.exit(1); });
