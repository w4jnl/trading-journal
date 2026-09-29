// Data-quality strip renderer shared by the GEX Overview and Exposure pages
// (engine update 009). Mirrors the gex_quality_strip template so a scrub
// slot repaints exactly what a page load renders. Engine strings (codes,
// messages) only ever land in textContent - never innerHTML.
(function () {
  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }
  function pill(cls, text, title) {
    var p = el('span', 'pill ' + cls, text);
    if (title) p.title = title;
    return p;
  }
  function render(host, q) {
    if (!host) return;
    host.textContent = '';
    if (!q || !q.show) { host.hidden = true; return; }
    host.hidden = false;
    if (q.degraded) {
      host.appendChild(pill('error', 'degraded',
        'At least one degraded finding: the percentile, the 1h/1d changes and the flip times of this row are unreliable'));
    }
    if (q.kind) host.appendChild(pill('warn', q.kind));
    (q.warnings || []).forEach(function (w) {
      var item = el('span', 'gex-q-item');
      item.appendChild(pill(w['class'] || 'warn', w.code + (w.symbol ? ' · ' + w.symbol : '')));
      if (w.message) item.appendChild(el('span', 'muted', ' ' + w.message));
      host.appendChild(item);
    });
    if (q.kindNote) host.appendChild(el('span', 'muted gex-q-note', q.kindNote));
  }
  window.gexQuality = { render: render };
})();
