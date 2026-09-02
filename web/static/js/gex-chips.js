/* Shared expiry-chip renderer for the /gex pages.
 *
 * The Exposure tab server-renders its initial strip (gex_exposure.html) and
 * calls this to REBUILD it per scrub slot - the expiry universe changes day
 * to day, so the chips must follow the viewed moment. The Overview page
 * (PR2) renders its strip entirely through this. Markup and classes mirror
 * the server template exactly; keep the two in sync.
 *
 * All content is set via textContent - expiry data is engine-sourced, but
 * the renderer must not become an injection sink regardless.
 */
(function () {
  'use strict';

  // One chip. exp: {date,label,dte,net,pct,neg}; modes: 'live' (multi-select
  // filter button), 'prof' (legacy 0DTE profile-switch button), 'locked'
  // (informational span), 'ghost' (selected date absent from this book).
  function buildChip(exp, mode, opts) {
    var isBtn = mode !== 'locked';
    var el = document.createElement(isBtn ? 'button' : 'span');
    if (isBtn) el.type = 'button';
    var cls = 'gxe-chip';
    if (mode === 'locked') cls += ' gxe-chip--locked';
    else cls += ' gxe-chip--live';
    if (mode === 'ghost') cls += ' gxe-chip--ghost on';
    cls += exp.neg ? ' gxe-chip--neg' : ' gxe-chip--pos';
    el.className = cls;

    if (mode === 'live' || mode === 'ghost') el.dataset.chipExp = exp.date;
    if (mode === 'prof') el.dataset.chipProf = '0dte';

    switch (mode) {
      case 'live':
        el.title = 'Toggle the ' + exp.label + ' expiry in the filter — ' + exp.net +
          ' rolls off (' + exp.pct + ' of decay); select several days to combine them';
        break;
      case 'ghost':
        el.title = 'Selected, but not in the book at this moment — click to drop it from the filter';
        break;
      case 'prof':
        el.title = "Today's expiry = the 0DTE profile — click to filter";
        break;
      default:
        el.title = 'Informational only — per-expiry strike filtering is not served by the engine yet (rolls off ' + exp.net + ')';
    }

    var b = document.createElement('b');
    b.textContent = mode === 'ghost' ? exp.label : exp.label + ' · ' + exp.dte + 'd';
    el.appendChild(b);
    if (!opts.compact) {
      var nums = document.createElement('span');
      nums.className = 'gxe-chip-nums';
      var num = document.createElement('span');
      num.className = 'num';
      num.textContent = exp.net;
      var pct = document.createElement('span');
      pct.className = 'gxe-pct';
      pct.textContent = exp.pct;
      nums.appendChild(num);
      nums.appendChild(pct);
      el.appendChild(nums);
    }
    return el;
  }

  window.gexChips = {
    // Rebuild host's children from expiries (server chip objects), keeping
    // `selected` (array of ISO dates) marked. Selected dates the book does
    // not offer are appended as ghost chips so they stay droppable. opts:
    // supportsExpiry (false = legacy mode: only today's chip is live),
    // compact (drop the numbers row - the Overview header variant).
    render: function (host, expiries, selected, opts) {
      opts = opts || {};
      var frag = document.createDocumentFragment();
      var have = {};
      (expiries || []).forEach(function (exp) {
        have[exp.date] = true;
        var mode;
        if (opts.supportsExpiry === false) mode = exp.dte === 0 ? 'prof' : 'locked';
        else mode = 'live';
        var el = buildChip(exp, mode, opts);
        if (mode === 'live' && selected.indexOf(exp.date) !== -1) el.classList.add('on');
        frag.appendChild(el);
      });
      if (opts.supportsExpiry !== false) {
        (selected || []).forEach(function (d) {
          if (have[d]) return;
          frag.appendChild(buildChip({ date: d, label: d, dte: 0, net: '—', pct: '', neg: false }, 'ghost', opts));
        });
      }
      host.textContent = '';
      host.appendChild(frag);
    },
  };
})();
