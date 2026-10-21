// Settings > AI > Trader profile: the focus list ranks by tick order. A
// ticked item moves below the last ticked one, an unticked item to the top
// of the unticked rest, and the rank badges renumber; the form posts the
// list in DOM order, which is the rank. Without this script the list posts
// in display order, still a valid ranking.
(function () {
  var list = document.getElementById('ai-focus');
  if (!list) return;
  function items() { return Array.prototype.slice.call(list.querySelectorAll('li')); }
  function box(li) { return li.querySelector('input[type=checkbox]'); }
  function renumber() {
    var n = 0;
    items().forEach(function (li) {
      var badge = li.querySelector('.ai-focus-rank');
      if (badge) badge.textContent = box(li).checked ? (++n) + '.' : '';
    });
  }
  list.addEventListener('change', function (e) {
    var cb = e.target;
    if (!cb || cb.type !== 'checkbox') return;
    var li = cb.closest('li');
    var checked = items().filter(function (x) { return x !== li && box(x).checked; });
    var anchor = checked.length ? checked[checked.length - 1] : null;
    if (anchor) anchor.insertAdjacentElement('afterend', li);
    else list.insertBefore(li, list.firstChild);
    renumber();
  });
  renumber();
})();
