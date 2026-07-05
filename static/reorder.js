/* Arrow-button row reordering, shared by the admin dashboard, the settings
 * default-question manager, and the setup wizard.
 *
 * Rows opt in with buttons carrying data-move="up" / data-move="down".
 *
 *   const reorder = wireReorder(list, {
 *       rowSelector: '.pl-qadmin-row',
 *       onMoved(rows) {},   // optional: after every move/refresh, rows in order
 *       persist() {},       // optional async save; calls are serialized on an
 *                           // internal promise chain so rapid clicks can't
 *                           // commit out of order server-side
 *   });
 *   reorder.refresh();      // recompute arrow disabled states (e.g. after a
 *                           // row is added or removed)
 */
function wireReorder(list, opts) {
    let queue = Promise.resolve();

    const rows = () => [...list.querySelectorAll(opts.rowSelector)];

    function refresh() {
        const all = rows();
        all.forEach((row, i) => {
            const up = row.querySelector('[data-move="up"]');
            const down = row.querySelector('[data-move="down"]');
            if (up) up.disabled = i === 0;
            if (down) down.disabled = i === all.length - 1;
        });
        if (opts.onMoved) opts.onMoved(all);
    }

    list.addEventListener('click', ev => {
        const btn = ev.target.closest('[data-move]');
        if (!btn || btn.disabled || !list.contains(btn)) return;

        // Rows may be <label>s — don't let the click toggle their checkbox.
        ev.preventDefault();

        const row = btn.closest(opts.rowSelector);
        const sibling = btn.dataset.move === 'up'
            ? row.previousElementSibling
            : row.nextElementSibling;
        if (!sibling || !sibling.matches(opts.rowSelector)) return;

        if (btn.dataset.move === 'up') sibling.before(row);
        else sibling.after(row);

        refresh();

        if (opts.persist) queue = queue.then(opts.persist).catch(() => {});
    });

    return { refresh };
}
