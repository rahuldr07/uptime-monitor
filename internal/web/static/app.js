(function () {
  'use strict';
  var $ = function (s, r) { return (r || document).querySelector(s); };
  var $$ = function (s, r) { return Array.prototype.slice.call((r || document).querySelectorAll(s)); };

  // ---- theme ----
  window.setTheme = function (t) {
    if (t === 'auto') { document.documentElement.removeAttribute('data-theme'); }
    else { document.documentElement.setAttribute('data-theme', t); }
    document.cookie = 'theme=' + t + ';path=/;max-age=31536000;samesite=lax';
    $$('[data-theme-label]').forEach(function (el) { el.textContent = t === 'auto' ? 'Auto' : t === 'dark' ? 'Dark' : 'Light'; });
    $$('[data-theme-icon]').forEach(function (el) { // sun when light, moon when dark, sun-moon when auto
      var cur = $('.ic:not(.swap-out)', el); if (cur) swapIcon(cur, t === 'auto' ? 'i-monitor' : t === 'dark' ? 'i-moon' : 'i-sun', 200);
    });
  };
  window.cycleTheme = function () {
    var cur = document.documentElement.getAttribute('data-theme') || 'auto';
    setTheme(cur === 'auto' ? 'dark' : cur === 'dark' ? 'light' : 'auto');
  };

  // ---- toasts ----
  // Live regions are created once at load (a region only announces what is inserted after it exists):
  // polite for ok/info, assertive for errors. Dismissal is the end of the CSS countdown on .toast-bar,
  // so pausing that animation (hover/focus) pauses dismissal. Errors have no bar and stay until closed.
  var MAX_TOASTS = 4;
  var reduced = function () { return window.matchMedia && matchMedia('(prefers-reduced-motion: reduce)').matches; };
  function toastRegions() {
    var h = $('.toasts');
    if (!h) { h = document.createElement('div'); h.className = 'toasts'; document.body.appendChild(h); }
    if (!h._live) {
      h._live = true; h.textContent = '';
      h.setAttribute('role', 'region'); h.setAttribute('aria-label', 'Notifications');
      var p = document.createElement('div'); p.className = 'toasts-live'; p.setAttribute('aria-live', 'polite');
      var a = document.createElement('div'); a.className = 'toasts-live'; a.setAttribute('role', 'alert'); a.setAttribute('aria-live', 'assertive');
      h.appendChild(p); h.appendChild(a); h._polite = p; h._assertive = a;
    }
    return h;
  }
  function dismissToast(t) {
    if (!t.parentNode || t._leaving) return;
    t._leaving = true; t.classList.remove('pulse'); t.classList.add('leaving');
    setTimeout(function () { t.remove(); }, reduced() ? 0 : 120);
  }
  function restartAnim(el, cls) {
    if (cls) { el.classList.remove(cls); void el.getBoundingClientRect(); el.classList.add(cls); return; }
    el.style.animation = 'none'; void el.getBoundingClientRect(); el.style.animation = '';
  }
  window.toast = function (msg, kind) {
    kind = kind || '';
    var h = toastRegions(), live = kind === 'error' ? h._assertive : h._polite;
    var all = $$('.toast', h).filter(function (x) { return !x._leaving; });
    for (var i = 0; i < all.length; i++) {
      if (all[i]._msg === msg && all[i]._kind === kind) { // duplicate: restart the clock, pulse once
        var bar = $('.toast-bar', all[i]); if (bar) restartAnim(bar);
        restartAnim(all[i], 'pulse'); return;
      }
    }
    var t = document.createElement('div');
    t.className = 'toast' + (kind ? ' ' + kind : ''); t._msg = msg; t._kind = kind;
    t.appendChild(icon(kindIcon(kind)));
    var m = document.createElement('span'); m.className = 'toast-msg'; m.textContent = msg; t.appendChild(m);
    var x = document.createElement('button'); x.type = 'button'; x.className = 'toast-x'; x.setAttribute('aria-label', 'Dismiss');
    x.appendChild(icon('i-x')); x.addEventListener('click', function () { dismissToast(t); });
    t.appendChild(x);
    if (kind !== 'error') {
      var b = document.createElement('span'); b.className = 'toast-bar'; b.setAttribute('aria-hidden', 'true');
      b.addEventListener('animationend', function () { dismissToast(t); });
      t.appendChild(b);
    }
    all.push(t);
    while (all.length > MAX_TOASTS) { // drop the oldest non-error, else the oldest
      var pick = null;
      for (var j = 0; j < all.length - 1; j++) { if (all[j]._kind !== 'error') { pick = all[j]; break; } }
      if (!pick) pick = all[0];
      all.splice(all.indexOf(pick), 1); pick.remove();
    }
    live.appendChild(t);
  };
  function liftFlash() {
    $$('[data-flash]').forEach(function (el) {
      toast(el.textContent.trim(), el.getAttribute('data-flash') === 'error' ? 'error' : '');
      el.remove();
    });
    if (location.search.indexOf('msg=') >= 0) {
      var u = new URL(location.href); u.searchParams.delete('msg'); u.searchParams.delete('sig');
      history.replaceState(null, '', u.pathname + (u.search || '') + u.hash);
    }
  }

  // ---- confirm dialogs ----
  function wireConfirms(root) {
    $$('form[data-confirm]', root).forEach(function (f) {
      if (f._wired) return; f._wired = true;
      f.addEventListener('submit', function (e) {
        if (f._ok) return;
        e.preventDefault();
        var d = $('#confirm');
        if (!d || !d.showModal) { if (confirm(f.getAttribute('data-confirm'))) { f._ok = true; f.submit(); } return; }
        var danger = f.hasAttribute('data-confirm-danger') || !!f.querySelector('button.danger');
        var opener = e.submitter || document.activeElement;
        $('#confirm-title').textContent = f.getAttribute('data-confirm-title') || 'Are you sure?';
        $('#confirm-msg').textContent = f.getAttribute('data-confirm');
        var head = $('.dlg-head', d), yes = $('button[value=yes]', d);
        head.classList.toggle('danger', danger);
        $('use', head).setAttribute('href', danger ? '#i-warn' : '#i-info');
        yes.className = danger ? 'danger' : 'primary';
        d.returnValue = '';
        d.showModal();
        d.onclose = function () {
          d.onclose = null;
          if (d.returnValue === 'yes') { f._ok = true; f.submit(); }
          else if (opener && opener.focus && document.contains(opener)) opener.focus();
        };
      });
    });
    var dlg = $('#confirm');
    if (dlg && !dlg._wired) { // a click whose target is the dialog itself landed on the backdrop (the dialog has no padding)
      dlg._wired = true;
      dlg.addEventListener('click', function (e) { if (e.target === dlg) { dlg.returnValue = 'no'; dlg.close(); } });
    }
  }

  // ---- async forms (fetch + JSON) ----
  function wireAsync(root) {
    $$('form[data-async]', root).forEach(function (f) {
      if (f._wired) return; f._wired = true;
      f.addEventListener('submit', function (e) {
        e.preventDefault();
        if (f._busy) return; f._busy = true;
        var btn = f.querySelector('button[type=submit]'); if (btn) btn.setAttribute('aria-busy', 'true');
        fetch(f.action, { method: 'POST', body: new FormData(f), headers: { 'Accept': 'application/json' }, credentials: 'same-origin' })
          .then(function (r) { if (r.status === 401) { sessionExpired(); throw 0; } return r.json().then(function (j) { j._status = r.status; return j; }); })
          .then(function (j) {
            if (j.error) { toast(j.error, 'error'); return; }
            if (j.message) toast(j.message, j.kind || (j.ok === false ? 'error' : 'ok')); // the pending check is not a success yet
            if (j.redirect) { location.href = j.redirect; } else { refresh(); }
          })
          .catch(function () { toast('Request failed', 'error'); })
          .then(function () { f._busy = false; if (btn) btn.removeAttribute('aria-busy'); });
      });
    });
  }

  // ---- slide-over panel ----
  function sessionExpired() { location.href = '/login?next=' + encodeURIComponent(location.pathname + location.search); }
  var panelOpener = null;
  window.openPanel = function () {
    var p = $('#panel'); if (!p) return;
    if (!document.body.classList.contains('panel-open')) {
      var a = document.activeElement; panelOpener = a && a !== document.body ? a : null;
    }
    document.body.classList.add('panel-open');
    var r = $('[data-test-result]', p); if (r) clearResult(r);
    var u = $('input[name=url]', p) || p; u.focus({ preventScroll: true });
  };
  window.closePanel = function () {
    if (!document.body.classList.contains('panel-open')) return;
    document.body.classList.remove('panel-open');
    var o = panelOpener; panelOpener = null;
    if (!o || !document.contains(o)) o = $('[data-action=open-panel]'); // no opener (opened with n), or it was replaced by a live refresh
    if (o && o.focus) o.focus();
  };
  function panelTab(e) {
    var p = $('#panel'); if (!p) return;
    var f = $$('a[href],button,input,select,textarea,summary,[tabindex]', p).filter(function (x) {
      return !x.disabled && x.tabIndex >= 0 && x.type !== 'hidden' && x.offsetParent !== null;
    });
    if (!f.length) { e.preventDefault(); p.focus(); return; }
    var a = document.activeElement, first = f[0], last = f[f.length - 1];
    if (!p.contains(a) || a === p) { e.preventDefault(); (e.shiftKey ? last : first).focus(); }
    else if (e.shiftKey && a === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && a === last) { e.preventDefault(); first.focus(); }
  }
  function icon(id) {
    var NS = 'http://www.w3.org/2000/svg', s = document.createElementNS(NS, 'svg'), u = document.createElementNS(NS, 'use');
    s.setAttribute('class', 'ic'); s.setAttribute('aria-hidden', 'true'); u.setAttribute('href', '#' + id); s.appendChild(u);
    return s;
  }
  // ok | error | warn | info -> sprite id
  function kindIcon(kind) { return kind === 'ok' ? 'i-check' : kind === 'error' ? 'i-alert' : kind === 'warn' ? 'i-warn' : 'i-info'; }
  // Replaces an icon with another one: the old svg fades out while the new one fades in on top of it (CSS .ic-swap).
  // Only the outer svgs animate; the symbols in the sprite are never touched. Reduced motion swaps the glyph in place.
  function swapIcon(old, id, ms) {
    var use = $('use', old);
    if (use && use.getAttribute('href') === '#' + id) return old;
    if (reduced() || !old.parentNode) { if (use) use.setAttribute('href', '#' + id); return old; }
    var wrap = old.parentNode;
    if (!wrap.classList.contains('ic-swap')) {
      wrap = document.createElement('span'); wrap.className = 'ic-swap';
      old.parentNode.insertBefore(wrap, old); wrap.appendChild(old);
    }
    var nu = icon(id); nu.classList.add('swap-in'); old.classList.remove('swap-in'); old.classList.add('swap-out'); wrap.appendChild(nu);
    setTimeout(function () { old.remove(); nu.classList.remove('swap-in'); }, ms || 150);
    return nu;
  }
  function clearResult(r) { r.className = 'alert'; r.hidden = true; r.textContent = ''; }
  // kind: ok | error | info; title is shown bold above detail when both are given
  function showResult(r, kind, title, detail) {
    r.className = 'alert ' + kind; r.textContent = '';
    var d = document.createElement('div');
    if (detail) { var b = document.createElement('strong'); b.textContent = title; d.appendChild(b); d.appendChild(document.createTextNode(detail)); }
    else d.textContent = title;
    r.appendChild(icon(kindIcon(kind))); r.appendChild(d);
    r.hidden = false;
  }
  // Pending state of an action: a skeleton bar (cloned from #skel-line) beside the label.
  function showPending(r, label) {
    r.className = 'alert info pending'; r.textContent = '';
    var t = $('#skel-line'), d = mk('div', 'pending-row'), l = mk('span');
    l.textContent = label;
    if (t) d.appendChild(t.content.cloneNode(true));
    d.appendChild(l); r.appendChild(d); r.hidden = false;
  }
  function wirePanel() {
    var f = $('#panel form'); if (!f || f._wired) return; f._wired = true;
    var url = f.querySelector('input[name=url]'), name = f.querySelector('input[name=name]');
    var res = function () { return f.querySelector('[data-test-result]'); };
    url.addEventListener('blur', function () {
      if (name.value.trim() || !url.value.trim()) return;
      try {
        var h = new URL(url.value.trim()).hostname.replace(/^www\./, ''); name.value = h.charAt(0).toUpperCase() + h.slice(1);
        name.dispatchEvent(new Event('input', { bubbles: true })); // clears a stale "Required." once a failed submit has been seen
      } catch (e) { }
    });
    var tb = f.querySelector('#test-btn'); if (tb) tb.addEventListener('click', function () {
      if (tb.disabled) return;
      var r = res(); showPending(r, 'Testing…');
      tb.disabled = true; tb.setAttribute('aria-busy', 'true');
      fetch('/targets/test', { method: 'POST', body: new FormData(f), headers: { 'Accept': 'application/json' }, credentials: 'same-origin' })
        .then(function (x) { if (x.status === 401) { sessionExpired(); throw 0; } return x.json(); })
        .then(function (j) {
          if (j.error && !j.status_code) { showResult(r, 'error', j.error); return; }
          showResult(r, j.ok ? 'ok' : 'error', j.ok ? 'Test passed' : 'Test failed',
            (j.status_code ? 'HTTP ' + j.status_code + ' ' : '') + 'in ' + j.latency_ms + ' ms' + (j.error ? ' · ' + j.error : ''));
        })
        .catch(function () { showResult(r, 'error', 'Could not run the test'); })
        .then(function () { tb.disabled = false; tb.removeAttribute('aria-busy'); });
    });
    f.addEventListener('submit', function (e) {
      e.preventDefault();
      if (f._busy) return; f._busy = true;
      var sb = f.querySelector('button[type=submit]'); if (sb) sb.setAttribute('aria-busy', 'true');
      fetch(f.action, { method: 'POST', body: new FormData(f), headers: { 'Accept': 'application/json' }, credentials: 'same-origin' })
        .then(function (x) { if (x.status === 401) { sessionExpired(); throw 0; } return x.json(); })
        .then(function (j) {
          if (j.error) { showResult(res(), 'error', j.error); return; }
          closePanel(); f.reset();
          if (j.redirect) location.href = j.redirect; else refresh();
        })
        .catch(function () { toast('Could not create the monitor', 'error'); })
        .then(function () { f._busy = false; if (sb) sb.removeAttribute('aria-busy'); });
    });
  }

  // ---- dropdowns ----
  // Enhances every <select>: the native select stays in the DOM (visually hidden, aria-hidden, out of the tab
  // order) and remains the source of truth, so forms, reset, dirty tracking and data-autosubmit keep working.
  // The menu is built on open and portaled to <body> (position:fixed) so no overflow ancestor can clip it.
  var ddN = 0, openDD = null;
  function mk(tag, cls) { var e = document.createElement(tag); if (cls) e.className = cls; return e; }
  function syncDD(d) {
    var o = d.s.options[d.s.selectedIndex];
    d.val.textContent = o ? o.text : ''; d.val.title = o ? o.text : ''; // the button truncates a long value
    d.btn.disabled = d.s.disabled;
  }
  function normQ(t) { return t.trim().toLowerCase().replace(/_/g, ' ').replace(/\u2212/g, '-'); } // zone names read with spaces and a minus sign
  function optText(d, i) { return normQ(d.s.options[i].text); }
  function selectable(d, i) { var li = d.items[i]; return !li.hidden && li.getAttribute('aria-disabled') !== 'true'; }
  function setHi(d, i, scroll) {
    if (d.hi >= 0 && d.items[d.hi]) d.items[d.hi].classList.remove('hl');
    d.hi = i;
    var holder = d.filter || d.btn;
    if (i < 0) { holder.removeAttribute('aria-activedescendant'); return; }
    var li = d.items[i]; li.classList.add('hl'); holder.setAttribute('aria-activedescendant', li.id);
    if (scroll) {
      var l = d.list, t = li.offsetTop, b = t + li.offsetHeight;
      if (t < l.scrollTop) l.scrollTop = t; else if (b > l.scrollTop + l.clientHeight) l.scrollTop = b - l.clientHeight;
    }
  }
  function moveHi(d, dir, edge) {
    var n = d.items.length, i = edge ? (dir > 0 ? -1 : n) : d.hi;
    for (;;) { i += dir; if (i < 0 || i >= n) return; if (selectable(d, i)) { setHi(d, i, true); return; } }
  }
  function placeDD(d) {
    var r = d.btn.getBoundingClientRect(), m = d.menu, vw = window.innerWidth, ih = window.innerHeight, vv = window.visualViewport,
      vh = vv ? Math.min(ih, vv.offsetTop + vv.height) : ih; // bottom of what is visible: the on-screen keyboard shrinks the visual viewport
    m.style.maxHeight = '280px'; m.style.top = '0'; m.style.bottom = 'auto'; m.style.left = '0'; m.style.minWidth = r.width + 'px';
    var h = m.offsetHeight, w = m.offsetWidth, below = vh - r.bottom - 8, above = r.top - 8;
    if (h > below && above > below) { // flip upward, anchored at the button so filtering can shrink it in place
      m.style.top = 'auto'; m.style.bottom = (ih - r.top + 4) + 'px'; m.style.maxHeight = Math.min(280, above - 4) + 'px';
    } else { m.style.top = (r.bottom + 4) + 'px'; m.style.maxHeight = Math.min(280, Math.max(below - 4, 80)) + 'px'; }
    m.style.left = Math.max(8, Math.min(d.alignEnd ? r.right - w : r.left, vw - 8 - w)) + 'px';
  }
  function applyFilter(d) {
    var q = normQ(d.filter.value), any = false;
    d.items.forEach(function (li, i) { var hide = !!q && optText(d, i).indexOf(q) < 0; li.hidden = hide; if (!hide) any = true; });
    d.none.hidden = any;
    if (d.hi < 0 || !selectable(d, d.hi)) { setHi(d, -1); moveHi(d, 1, true); }
    placeDD(d);
  }
  function typeAhead(d, ch) {
    var now = Date.now(); if (now - d.qt > 500) d.q = ''; d.qt = now; d.q += ch.toLowerCase();
    var n = d.items.length, start = d.hi < 0 ? 0 : (d.q.length === 1 ? d.hi + 1 : d.hi);
    for (var k = 0; k < n; k++) {
      var i = (start + k) % n;
      if (selectable(d, i) && optText(d, i).indexOf(d.q) === 0) { setHi(d, i, true); return; }
    }
  }
  function closeMenu(d, refocus) {
    if (!d.open) return;
    if (d.close) { d.close(refocus); return; } // a plain popup (View menu)
    d.open = false; if (openDD === d) openDD = null;
    d.menu.remove(); d.menu = d.list = d.filter = d.none = null; d.items = [];
    d.wrap.classList.remove('open'); d.btn.setAttribute('aria-expanded', 'false'); d.btn.removeAttribute('aria-activedescendant');
    if (refocus) d.btn.focus();
  }
  function commit(d, i) {
    var s = d.s, o = s.options[i]; if (!o || o.disabled) return;
    var changed = s.selectedIndex !== i; s.selectedIndex = i; syncDD(d);
    closeMenu(d, true);
    if (changed) { s.dispatchEvent(new Event('input', { bubbles: true })); s.dispatchEvent(new Event('change', { bubbles: true })); }
  }
  function openMenu(d, seed) {
    var s = d.s; if (s.disabled || d.open) return;
    if (openDD) closeMenu(openDD, false);
    syncDD(d);
    var menu = mk('div', 'dd-menu'), list = mk('ul', 'dd-list');
    menu.id = d.id + '-menu'; list.id = d.id + '-list'; list.setAttribute('role', 'listbox'); list.setAttribute('aria-label', d.name);
    d.items = []; d.hi = -1; d.q = ''; d.qt = 0; d.filter = null;
    if (s.options.length > 8) {
      var f = mk('input', 'dd-filter'); f.type = 'text'; f.setAttribute('role', 'searchbox'); f.setAttribute('aria-label', 'Filter options');
      f.setAttribute('aria-controls', list.id); f.setAttribute('autocomplete', 'off'); f.setAttribute('spellcheck', 'false');
      f.addEventListener('input', function () { applyFilter(d); });
      menu.appendChild(f); d.filter = f;
    }
    Array.prototype.forEach.call(s.options, function (o, i) {
      var li = mk('li', 'dd-opt'), sp = mk('span'); li.id = d.id + '-o' + i; li.setAttribute('role', 'option'); li.setAttribute('data-i', i);
      li.setAttribute('aria-selected', i === s.selectedIndex ? 'true' : 'false');
      if (o.disabled || (o.parentNode.tagName === 'OPTGROUP' && o.parentNode.disabled)) li.setAttribute('aria-disabled', 'true');
      sp.textContent = o.text || ' '; li.appendChild(icon('i-check')); li.appendChild(sp);
      list.appendChild(li); d.items.push(li);
    });
    var none = mk('div', 'dd-none'); none.textContent = 'No matches'; none.hidden = true; d.none = none;
    menu.appendChild(list); menu.appendChild(none); document.body.appendChild(menu);
    d.menu = menu; d.list = list; d.open = true; openDD = d; d.sy = window.scrollY;
    d.wrap.classList.add('open'); d.btn.setAttribute('aria-expanded', 'true'); d.btn.setAttribute('aria-controls', list.id);
    menu.addEventListener('mousedown', function (e) { if (e.target !== d.filter) e.preventDefault(); }); // keep focus where it is
    menu.addEventListener('click', function (e) { var li = e.target.closest('.dd-opt'); if (li && li.getAttribute('aria-disabled') !== 'true') commit(d, +li.getAttribute('data-i')); });
    menu.addEventListener('mouseover', function (e) { var li = e.target.closest('.dd-opt'); if (li && li.getAttribute('aria-disabled') !== 'true') setHi(d, +li.getAttribute('data-i'), false); });
    menu.addEventListener('keydown', function (e) { ddKey(d, e); });
    menu.addEventListener('focusout', function (e) { if (d.open && e.relatedTarget !== d.btn && !menu.contains(e.relatedTarget)) closeMenu(d, false); });
    placeDD(d);
    d.items.forEach(function (li) { var t = li.lastChild; if (t.scrollWidth > t.clientWidth) t.title = t.textContent; }); // a label cut by the menu's width keeps its full text as a tooltip
    var sel = s.selectedIndex >= 0 && selectable(d, s.selectedIndex) ? s.selectedIndex : -1;
    if (sel >= 0) setHi(d, sel, true); else moveHi(d, 1, true);
    if (d.filter) {
      d.filter.focus({ preventScroll: true });
      if (seed) { d.filter.value = seed; applyFilter(d); }
    } else if (seed) typeAhead(d, seed);
  }
  function ddKey(d, e) {
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    var k = e.key, inF = e.target === d.filter;
    if (!d.open) {
      if (k === 'Enter' || k === ' ' || k === 'ArrowDown' || k === 'ArrowUp') { e.preventDefault(); openMenu(d); }
      else if (k.length === 1) { e.preventDefault(); openMenu(d, k); }
      return;
    }
    if (k === 'ArrowDown' || k === 'ArrowUp') { e.preventDefault(); moveHi(d, k === 'ArrowDown' ? 1 : -1); }
    else if ((k === 'Home' || k === 'End') && !inF) { e.preventDefault(); moveHi(d, k === 'Home' ? 1 : -1, true); }
    else if (k === 'Enter' || (k === ' ' && !inF && (!d.q || Date.now() - d.qt > 500))) { e.preventDefault(); d.q = ''; if (d.hi >= 0) commit(d, d.hi); else closeMenu(d, true); }
    else if (k === 'Escape') { e.preventDefault(); e.stopPropagation(); closeMenu(d, true); }
    else if (k === 'Tab') { if (d.hi >= 0) commit(d, d.hi); else closeMenu(d, true); } // focus is on the button again, so Tab moves on from it
    else if (k.length === 1 && !inF) { e.preventDefault(); typeAhead(d, k); }
  }
  function enhanceSelect(s) {
    var d = { s: s, id: 'dd' + (++ddN), open: false, hi: -1, q: '', qt: 0, items: [] };
    s._dd = d;
    var wrap = mk('div', 'dd' + (s.style.width === 'auto' ? ' auto' : '')), btn = mk('button', 'dd-btn'), val = mk('span', 'dd-val');
    btn.type = 'button'; btn.id = d.id + '-btn'; btn.setAttribute('role', 'combobox'); btn.setAttribute('aria-haspopup', 'listbox');
    btn.setAttribute('aria-expanded', 'false'); btn.setAttribute('aria-controls', d.id + '-list');
    btn.appendChild(val); btn.appendChild(icon('i-chevron'));
    d.wrap = wrap; d.btn = btn; d.val = val; s._ddBtn = btn; btn._sel = s;
    s.parentNode.insertBefore(wrap, s); wrap.appendChild(btn); wrap.appendChild(s);
    s.tabIndex = -1; s.setAttribute('aria-hidden', 'true'); s.classList.add('dd-native');
    var name = s.getAttribute('aria-label') || '';
    if (s.id) $$('label[for]').forEach(function (l) { if (l.htmlFor === s.id) { l.htmlFor = btn.id; name = name || l.textContent.trim(); } });
    var wl = s.closest('label');
    if (wl) { if (!wl.id) wl.id = d.id + '-lbl'; btn.setAttribute('aria-labelledby', wl.id); name = name || wl.textContent.trim(); }
    else if (s.getAttribute('aria-label')) btn.setAttribute('aria-label', s.getAttribute('aria-label'));
    d.name = name || 'Options';
    btn.addEventListener('click', function () { if (d.open) closeMenu(d, true); else { btn.focus({ preventScroll: true }); openMenu(d); } }); // focus first: Safari does not focus buttons on click
    btn.addEventListener('keydown', function (e) { ddKey(d, e); });
    btn.addEventListener('keyup', function (e) { if (e.key === ' ') e.preventDefault(); }); // Space must not click on release
    btn.addEventListener('focusout', function (e) { if (d.open && !d.menu.contains(e.relatedTarget)) closeMenu(d, false); });
    if (s.form) s.form.addEventListener('reset', function () { setTimeout(function () { syncDD(d); }, 0); });
    syncDD(d);
  }
  function wireDropdowns(root) {
    $$('select', root).forEach(function (s) {
      if (s._dd || s.hasAttribute('data-native') || s.multiple || s.size > 1) return;
      enhanceSelect(s);
    });
  }
  document.addEventListener('mousedown', function (e) {
    var d = openDD; if (d && !d.menu.contains(e.target) && !d.btn.contains(e.target)) closeMenu(d, false);
  }, true);
  window.addEventListener('scroll', function (e) {
    var d = openDD; if (!d || d.menu.contains(e.target)) return;
    if (e.target === document && window.scrollY === d.sy) return; // a late scroll event from before the menu opened (the page did not move since)
    closeMenu(d, false);
  }, true);
  window.addEventListener('resize', function () { if (openDD) closeMenu(openDD, false); });
  if (window.visualViewport) window.visualViewport.addEventListener('resize', function () { if (openDD && openDD.menu && openDD.filter) placeDD(openDD); }); // the keyboard opened or closed: keep the menu in view

  // ---- data tables ----
  // Progressive enhancement over a server-rendered <table> inside .dt.
  // Rows stay in the DOM: filtering and paging set tr.hidden, sorting reorders the <tbody>. State (query, pill,
  // sort, page) lives in dtState keyed by data-table, so a live refresh that replaces #content re-applies it.
  var DT_KEY = 'uptime.tables', DT_SIZES = [25, 50, 100], dtPrefs = null, dtState = {}, dtLive = [];
  var DT_MIN = 64, DT_FLOOR = 8, DT_MAX = 640, dtDragging = false;
  function isRec(x) { return !!x && typeof x === 'object' && !Array.isArray(x); }
  function readPrefs() {
    var p = { density: '', size: 25, columns: {}, widths: {} }, raw = null;
    try { raw = JSON.parse(localStorage.getItem(DT_KEY) || 'null'); } catch (e) { raw = null; }
    if (!isRec(raw)) return p;
    if (raw.density === 'compact' || raw.density === 'comfortable') p.density = raw.density;
    if (DT_SIZES.indexOf(raw.size) >= 0) p.size = raw.size;
    if (isRec(raw.columns)) Object.keys(raw.columns).forEach(function (t) {
      if (!isRec(raw.columns[t])) return;
      var o = {}; Object.keys(raw.columns[t]).forEach(function (c) { if (typeof raw.columns[t][c] === 'boolean') o[c] = raw.columns[t][c]; });
      p.columns[t] = o;
    });
    if (isRec(raw.widths)) Object.keys(raw.widths).forEach(function (t) {
      if (!isRec(raw.widths[t])) return;
      var o = {}; Object.keys(raw.widths[t]).forEach(function (c) { var w = raw.widths[t][c]; if (typeof w === 'number' && isFinite(w) && w >= DT_FLOOR && w <= DT_MAX) o[c] = w; });
      p.widths[t] = o;
    });
    return p;
  }
  function prefs() { return dtPrefs || (dtPrefs = readPrefs()); }
  function savePrefs() { try { localStorage.setItem(DT_KEY, JSON.stringify(prefs())); } catch (e) { } }

  var NUM_RE = /^-?(\d+\.?\d*|\.\d+)$/;
  // Sort key: explicit key, else the cell text; "" and an em dash are empty; numeric text becomes a number
  function sortVal(raw) {
    if (raw === null || raw === undefined) return undefined;
    var t = String(raw).replace(/\s+/g, ' ').trim();
    if (!t || t === '—') return undefined;
    var c = t.replace(/[$₹,\s]/g, '').replace('−', '-');
    return NUM_RE.test(c) ? Number(c) : t;
  }
  function cmpVal(a, b) {
    var na = typeof a === 'number', nb = typeof b === 'number';
    if (na && nb) return a - b;
    if (na) return -1;
    if (nb) return 1;
    return String(a).localeCompare(String(b), undefined, { numeric: true, sensitivity: 'base' });
  }
  // text a search can match: skips forms, buttons, inputs, icons and screen-reader-only text
  function cellText(el, out) {
    for (var n = el.firstChild; n; n = n.nextSibling) {
      if (n.nodeType === 3) out.push(n.nodeValue);
      else if (n.nodeType === 1 && !/^(FORM|BUTTON|INPUT|SELECT|TEXTAREA|SVG|SCRIPT|STYLE)$/i.test(n.tagName) && !n.classList.contains('sr')) cellText(n, out);
    }
    return out;
  }
  function pageList(index, count, slots) { // page-number list with ellipsis gaps
    var last = count - 1, side = slots - 2, near = (slots - 5) / 2, out = [], i;
    function run(a, b) { var r = []; for (var k = a; k <= b; k++) r.push(k); return r; }
    if (count <= slots) return run(0, last);
    if (index < side - 1) return run(0, side - 1).concat(['gap', last]);
    if (index > last - side + 1) return [0, 'gap'].concat(run(last - side + 1, last));
    out = [0, 'gap'].concat(run(index - near, index + near)); out.push('gap', last);
    return out;
  }
  function singular(n, noun) { return n === 1 ? noun.replace(/s$/, '') : noun; }
  function filterSummary(shown, total, noun, parts) { // result summary line naming the active filters
    var tail = parts.length ? ' ' + parts.join(', ') : '';
    if (shown === 0) return 'No ' + noun + tail + '.' + (parts.length ? ' Take a filter off to see more.' : '');
    return 'Showing ' + shown + ' of ' + total + ' ' + singular(total, noun) + (parts.length ? ', ' + parts.join(', ') : '') + '.';
  }
  function clampW(px, min) { return Math.ceil(Math.min(DT_MAX, Math.max(min || DT_MIN, px))); }
  function colMin(c) { return c.w && c.w < DT_MIN ? c.w : DT_MIN; } // a data-w below 64 (the overview status dot) is that column's own minimum
  function dtById(id) { return dtLive.filter(function (t) { return t.id === id; }).pop(); }
  // a bulk bar whose table is no longer on the page goes away (its Copy would read detached rows)
  function dropBulk() {
    $$('.dt-bulk').forEach(function (b) {
      var id = b.getAttribute('data-for');
      if (!$('.dt[data-table="' + id + '"]')) { b.remove(); if (dtState[id]) dtState[id].bulk = null; }
    });
  }
  function sameHref(h) { // a same-origin path only
    if (!h || !/^\/[^\/\\]/.test(h)) return false;
    try { return new URL(h, location.href).origin === location.origin; } catch (e) { return false; }
  }
  function rangeIds(ids, from, to) { // ids between two rows (shift-click range)
    var a = ids.indexOf(from), b = ids.indexOf(to);
    if (a < 0 || b < 0) return [to];
    return ids.slice(Math.min(a, b), Math.max(a, b) + 1);
  }
  function copyText(text, ok, fail) {
    var fallback = function () {
      var ta = mk('textarea'); ta.value = text; ta.setAttribute('readonly', ''); ta.style.position = 'fixed'; ta.style.opacity = '0';
      document.body.appendChild(ta); ta.select();
      var done = false; try { done = document.execCommand('copy'); } catch (e) { }
      ta.remove(); if (done) ok(); else fail();
    };
    if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(text).then(ok, fallback); else fallback();
  }
  function setSel(sel, v) { sel.value = v; if (sel._dd) syncDD(sel._dd); }

  // A small menu (View). Same look and closers as the dropdown popup: the .dd-menu class makes editing() hold
  // live refreshes and the shortcut guard ignore its keys. Focus moves into the menu; toggles keep it open.
  function openPop(btn, items) {
    if (btn._pm && btn._pm.open) { closeMenu(btn._pm, true); return; }
    if (openDD) closeMenu(openDD, false);
    var m = { btn: btn, open: true, alignEnd: true }, menu = mk('div', 'dd-menu pm'), btns = [];
    menu.setAttribute('role', 'menu'); menu.setAttribute('aria-label', btn.getAttribute('aria-label') || 'Menu');
    items.forEach(function (it) {
      if (it.t === 'sep') { menu.appendChild(mk('div', 'pm-sep')); return; }
      if (it.t === 'head') { var h = mk('div', 'pm-head'); h.textContent = it.label; menu.appendChild(h); return; }
      var b = mk('button', 'pm-item'), sp = mk('span');
      b.type = 'button'; b.tabIndex = -1; it.el = b;
      b.setAttribute('role', it.t === 'radio' ? 'menuitemradio' : it.t === 'check' ? 'menuitemcheckbox' : 'menuitem');
      if (it.t) b.setAttribute('aria-checked', it.on ? 'true' : 'false');
      sp.textContent = it.label; b.appendChild(icon('i-check')); b.appendChild(sp);
      b.addEventListener('click', function () {
        if (it.t === 'check') { it.on = !it.on; b.setAttribute('aria-checked', it.on ? 'true' : 'false'); it.run(it.on); }
        else if (it.t === 'radio') {
          items.forEach(function (x) { if (x.t === 'radio' && x.group === it.group) { x.on = x === it; x.el.setAttribute('aria-checked', x.on ? 'true' : 'false'); } });
          it.run();
        } else { it.run(); closeMenu(m, true); }
      });
      b.addEventListener('mouseover', function () { b.focus({ preventScroll: true }); });
      menu.appendChild(b); btns.push(b);
    });
    m.menu = menu; btn._pm = m; openDD = m; m.sy = window.scrollY;
    m.close = function (refocus) {
      if (!m.open) return;
      m.open = false; if (openDD === m) openDD = null;
      menu.remove(); btn.setAttribute('aria-expanded', 'false');
      if (refocus) btn.focus();
    };
    menu.addEventListener('keydown', function (e) {
      var i = btns.indexOf(document.activeElement), k = e.key;
      if (k === 'ArrowDown' || k === 'ArrowUp') { e.preventDefault(); btns[(i + (k === 'ArrowDown' ? 1 : btns.length - 1) + (i < 0 && k === 'ArrowUp' ? 1 : 0)) % btns.length].focus({ preventScroll: true }); }
      else if (k === 'Home' || k === 'End') { e.preventDefault(); btns[k === 'Home' ? 0 : btns.length - 1].focus({ preventScroll: true }); }
      else if (k === 'Escape') { e.preventDefault(); e.stopPropagation(); closeMenu(m, true); }
      else if (k === 'Tab') closeMenu(m, true);
    });
    menu.addEventListener('focusout', function (e) { if (m.open && e.relatedTarget !== btn && !menu.contains(e.relatedTarget)) closeMenu(m, false); });
    document.body.appendChild(menu);
    btn.setAttribute('aria-expanded', 'true');
    placeDD(m);
    var first = null; items.forEach(function (it) { if (!first && it.el && it.on) first = it.el; });
    (first || btns[0]).focus({ preventScroll: true });
  }

  function buildTable(root) {
    root.classList.add('dt-js'); // phone cards in the CSS need this; without JS the plain table scrolls
    var T = { root: root, id: root.getAttribute('data-table') || 'table', noun: root.getAttribute('data-noun') || 'rows' };
    var table = $('table', root), tbody = $('tbody', table), mode = root.getAttribute('data-paged') || 'client';
    var emptyText = root.getAttribute('data-empty') || 'Nothing to show.';
    var topbar = root.hasAttribute('data-topbar-search') && mode !== 'server' ? $('input[data-search]') : null;
    var searchLabel = mode === 'server' ? '' : root.getAttribute('data-search');
    var S = dtState[T.id] || (dtState[T.id] = { q: '', pill: 'all', sort: null, page: 0, sel: {} });
    T.topbar = topbar;
    if (topbar) { if (S.q && !topbar.value) topbar.value = S.q; else S.q = topbar.value; }

    // columns and rows, read from the markup
    T.rows = Array.prototype.filter.call(tbody.children, function (tr) { return tr.tagName === 'TR'; }).map(function (tr, n) {
      return { tr: tr, idx: n, k: (tr.getAttribute('data-k') || '').split(/\s+/).filter(Boolean), vals: [], text: null };
    });
    var select = root.hasAttribute('data-select'), lead = select ? 1 : 0, hcb = null;
    T.rows.forEach(function (r) { r.id = r.tr.getAttribute('data-id') || String(r.idx); r.tr._r = r; });
    if (select) { // a leading checkbox column; column index i of the markup is cell i + lead
      var hrow = $('thead tr', table), th0 = mk('th', 'dt-tick');
      hcb = mk('input'); hcb.id = 'dt-' + T.id + '-tickall'; hcb.type = 'checkbox'; th0.appendChild(hcb); hrow.insertBefore(th0, hrow.firstChild);
      T.rows.forEach(function (r) {
        var td = mk('td', 'dt-tick'), cb = mk('input'); cb.id = 'dt-' + T.id + '-tick-' + r.id; cb.type = 'checkbox'; td.appendChild(cb); r.tr.insertBefore(td, r.tr.firstChild); r.cb = cb;
      });
    }
    var ths = $$('thead th', table).slice(lead);
    function rowVal(r, i) {
      if (!(i in r.vals)) {
        var td = r.tr.children[i + lead];
        r.vals[i] = !td ? undefined : sortVal(td.hasAttribute('data-sort') ? td.getAttribute('data-sort') : td.textContent);
      }
      return r.vals[i];
    }
    T.cols = ths.map(function (th, i) {
      var label = th.textContent.replace(/\s+/g, ' ').trim();
      var c = {
        i: i, th: th, label: label, id: label || '#' + i, pin: th.getAttribute('data-pin') || '', hide: th.hasAttribute('data-hide'),
        w: +th.getAttribute('data-w') || 0, below: th.getAttribute('data-hide-below') || ''
      };
      c.canSort = th.getAttribute('data-sort') !== 'false' && T.rows.some(function (r) { return rowVal(r, i) !== undefined; });
      c.canHide = i > 0 && !c.pin && !!label;
      return c;
    });
    T.pk = T.id + ':' + T.cols.map(function (c) { return c.label; }).join('|');
    var colById = {}; T.cols.forEach(function (c) { colById[c.id] = c; });

    // column classes (breakpoint hiding and pinning are CSS)
    T.cols.forEach(function (c) {
      var cls = [];
      if (c.below) cls.push('hb-' + c.below);
      if (c.pin === 'end') cls.push('dt-pin-end');
      if (th_num(c.th)) cls.push('dt-num');
      c.cls = cls;
      cls.forEach(function (x) { c.th.classList.add(x); });
      T.rows.forEach(function (r) { var td = r.tr.children[c.i + lead]; if (td) cls.forEach(function (x) { td.classList.add(x); }); });
      c.th.setAttribute('scope', 'col');
      if (c.w) c.th.style.minWidth = c.w + 'px';
    });
    function th_num(th) { return th.hasAttribute('data-num'); }
    // phone cards (CSS does the layout): title column, up to 3 facts, actions; the rest is hidden under 640px
    (function () {
      var act = T.cols.filter(function (c) { return c.pin === 'end' || c.th.getAttribute('data-card') === 'actions'; })[0];
      var free = T.cols.filter(function (c) { return c !== act && c.th.getAttribute('data-card') !== 'hide'; });
      var prim = free.filter(function (c) { return c.th.getAttribute('data-card') === 'primary'; })[0] || free[0];
      var sec = free.filter(function (c) { return c !== prim && c.th.getAttribute('data-card') === 'secondary'; });
      if (!sec.length) sec = free.filter(function (c) { return c !== prim; });
      sec = sec.slice(0, 3);
      T.cols.forEach(function (c) {
        var role = c === act ? 'dt-ca' : c === prim ? 'dt-cp' : sec.indexOf(c) >= 0 ? 'dt-cs' : 'dt-cx';
        c.th.classList.add(role);
        T.rows.forEach(function (r) {
          var td = r.tr.children[c.i + lead]; if (!td) return;
          td.classList.add(role); if (role === 'dt-cs') td.setAttribute('data-label', c.label);
        });
      });
    })();
    T.rows.forEach(function (r) {
      var lbl = r.tr.getAttribute('data-label') || (T.cols.length ? cellText(r.tr.children[lead] || r.tr, []).join(' ').replace(/\s+/g, ' ').trim() : '');
      r.label = lbl || 'row ' + (r.idx + 1);
      if (r.cb) r.cb.setAttribute('aria-label', 'Select ' + r.label);
      if (r.tr.getAttribute('data-href')) r.tr.classList.add('dt-click');
    });

    // chrome
    var bar = mk('div', 'dt-bar'), sum = mk('p', 'dt-sum'), sumT = mk('span'), clear = mk('button', 'dt-link');
    var box = mk('div', 'dt-box'), wrap = mk('div', 'dt-wrap'), foot = mk('nav', 'dt-foot'), live = mk('span', 'sr');
    var filterable = !!(topbar || searchLabel || root.getAttribute('data-pills'));
    var q = null, pillBtns = [], more = null, sizeSel = null;
    if (searchLabel) {
      var sw = mk('div', 'search dt-search'); q = mk('input', 'dt-q'); q.type = 'search'; q.placeholder = searchLabel; q.setAttribute('aria-label', searchLabel);
      q.setAttribute('autocomplete', 'off'); q.value = S.q; q.id = 'dt-' + T.id + '-q';
      sw.appendChild(icon('i-search')); sw.appendChild(q); bar.appendChild(sw);
      q.addEventListener('input', function () { T.setQuery(q.value); });
    }
    var spec = (root.getAttribute('data-pills') || '').split(',').map(function (p) { var a = p.split(':'); return a[0] ? { key: a[0], label: a[1] || a[0] } : null; }).filter(Boolean);
    spec.forEach(function (p) { p.count = p.key === 'all' ? T.rows.length : T.rows.filter(function (r) { return r.k.indexOf(p.key) >= 0; }).length; });
    if (spec.length) {
      var out = spec.length > 5 ? spec.slice(0, 4) : spec, rest = spec.slice(out.length), pills = mk('div', 'dt-pills');
      pills.setAttribute('role', 'group'); pills.setAttribute('aria-label', 'Quick views');
      out.forEach(function (p) {
        var b = mk('button', 'dt-pill'), n = mk('span', 'dt-n'); b.type = 'button'; b.id = 'dt-' + T.id + '-pill-' + p.key; b.appendChild(document.createTextNode(p.label + ' ')); n.textContent = p.count; b.appendChild(n);
        b.addEventListener('click', function () { T.setPill(p.key); });
        b._key = p.key; pillBtns.push(b); pills.appendChild(b);
      });
      if (rest.length) {
        more = mk('select', 'dt-more'); more.id = 'dt-' + T.id + '-more'; more.setAttribute('aria-label', 'More views'); more.style.width = 'auto';
        var o0 = mk('option'); o0.value = ''; o0.text = 'More'; more.add(o0);
        rest.forEach(function (p) { var o = mk('option'); o.value = p.key; o.text = p.label + ' (' + p.count + ')'; more.add(o); });
        more.addEventListener('change', function () { T.setPill(more.value || out[0].key); });
        pills.appendChild(more);
      }
      bar.appendChild(pills);
    }
    var inline = !q && !spec.length; // nothing to filter: the View button joins the summary line instead of a bar of its own
    var grow = mk('span', 'dt-grow');
    var vb = mk('button', 'dt-view sm'), vlab = mk('span'); vb.type = 'button'; vb.id = 'dt-' + T.id + '-view'; vb.setAttribute('aria-haspopup', 'menu'); vb.setAttribute('aria-expanded', 'false'); vb.setAttribute('aria-label', 'View: density and columns');
    vlab.textContent = 'View'; vb.appendChild(icon('i-columns')); vb.appendChild(vlab);
    var vc = icon('i-chevron'); vc.classList.add('chev'); vb.appendChild(vc);
    vb.addEventListener('click', function () { vb.focus({ preventScroll: true }); T.viewMenu(); });
    vb.addEventListener('keydown', function (e) { if (e.key === 'ArrowDown') { e.preventDefault(); T.viewMenu(); } });
    (inline ? sum : bar).appendChild(grow); (inline ? sum : bar).appendChild(vb);

    sum.setAttribute('aria-live', 'polite'); sum.hidden = !filterable && !inline; sumT.hidden = !filterable;
    clear.type = 'button'; clear.id = 'dt-' + T.id + '-clear'; clear.textContent = 'Clear all'; clear.hidden = true;
    clear.addEventListener('click', function () { T.clearAll(); });
    sum.insertBefore(clear, sum.firstChild); sum.insertBefore(sumT, sum.firstChild);

    wrap.setAttribute('role', 'region'); wrap.tabIndex = 0; wrap.id = 'dt-' + T.id + '-region'; // an id so a refresh can restore focus to it
    wrap.setAttribute('aria-label', T.noun + '. Arrow keys move' + (select ? ', Space selects' : '') + (T.rows.some(function (r) { return r.tr.getAttribute('data-href'); }) ? ', Enter opens' : '') + '.');
    live.setAttribute('aria-live', 'polite');
    foot.setAttribute('aria-label', 'Pages of ' + T.noun); foot.hidden = true;
    var cnt = mk('p', 'dt-count'), size = mk('span', 'dt-size'), pg = mk('div', 'dt-pages');
    if (mode === 'client') {
      sizeSel = mk('select', 'dt-sizesel'); sizeSel.id = 'dt-' + T.id + '-size'; sizeSel.setAttribute('aria-label', 'Rows per page'); sizeSel.style.width = 'auto';
      DT_SIZES.forEach(function (n) { var o = mk('option'); o.value = n; o.text = n; sizeSel.add(o); });
      var sl = mk('span', 'dt-rows'); sl.textContent = 'Rows per page'; sl.setAttribute('aria-hidden', 'true');
      size.appendChild(sl); size.appendChild(sizeSel);
      sizeSel.addEventListener('change', function () { prefs().size = +sizeSel.value; savePrefs(); S.page = 0; dtLive.forEach(function (t) { if (t.sizeSel) t.render(); }); });
    }
    foot.appendChild(cnt); foot.appendChild(size); foot.appendChild(pg);

    table.parentNode.insertBefore(box, table); box.appendChild(wrap); wrap.appendChild(table);
    var edge = mk('span', 'dt-edge'), eb = mk('button', 'dt-edge-more'); edge.setAttribute('aria-hidden', 'true');
    eb.type = 'button'; eb.tabIndex = -1; eb.appendChild(document.createTextNode('More columns')); eb.appendChild(icon('i-chevron-right')); edge.appendChild(eb); box.appendChild(edge);
    eb.addEventListener('click', function () { wrap.scrollBy({ left: wrap.clientWidth * 0.8, behavior: 'smooth' }); });
    wrap.addEventListener('scroll', function () { T.edge(); }, { passive: true });
    if (!inline) root.insertBefore(bar, box); root.insertBefore(sum, box); root.appendChild(foot); root.appendChild(live);
    T.sizeSel = sizeSel;

    // headers: sort button, column menu, resize grip
    var ARROW = { asc: 'i-arrow-up', desc: 'i-arrow-down', none: 'i-sort' };
    function drag(c, g) {
      var d = null;
      function up() { if (!d) return; d = null; dtDragging = false; g.classList.remove('on'); savePrefs(); }
      g.addEventListener('pointerdown', function (e) {
        if (e.button) return; e.preventDefault(); e.stopPropagation();
        try { g.setPointerCapture(e.pointerId); } catch (x) { }
        d = { x: e.clientX, w: c.th.getBoundingClientRect().width }; dtDragging = true; g.classList.add('on');
      });
      g.addEventListener('pointermove', function (e) { if (!d) return; if (e.buttons === 0) { up(); return; } T.widen(c, d.w + e.clientX - d.x, false); });
      g.addEventListener('pointerup', up); g.addEventListener('pointercancel', up); g.addEventListener('lostpointercapture', up);
      g.addEventListener('click', function (e) { e.stopPropagation(); });
      g.addEventListener('dblclick', function () { T.fitCol(c); });
      g.addEventListener('keydown', function (e) {
        var by = e.key === 'ArrowRight' ? 16 : e.key === 'ArrowLeft' ? -16 : 0;
        if (by) T.widen(c, c.th.getBoundingClientRect().width + by);
        else if (e.key === 'Enter' || e.key === 'Delete') T.widen(c, undefined);
        else return;
        e.preventDefault(); e.stopPropagation();
      });
    }
    T.cols.forEach(function (c) {
      var th = c.th, span = mk('span', 'dt-th');
      if (c.canSort) {
        var b = mk('button', 'dt-sort'), sv = icon(ARROW.none);
        b.type = 'button'; b.id = 'dt-' + T.id + '-sort-' + c.i; b.appendChild(document.createTextNode(c.label));
        sv.classList.add('dt-arrow'); b.appendChild(sv);
        span.appendChild(b); c.arrow = sv; c.btn = b;
        b.addEventListener('click', function () { T.sortBy(c); });
      } else while (th.firstChild) span.appendChild(th.firstChild);
      th.textContent = ''; th.appendChild(span);
      if (c.pin) return; // the actions column has no menu or grip
      var hm = mk('button', 'dt-hm');
      hm.type = 'button'; hm.id = 'dt-' + T.id + '-colmenu-' + c.i; hm.setAttribute('aria-haspopup', 'menu'); hm.setAttribute('aria-expanded', 'false'); hm.setAttribute('aria-label', c.label + ' column menu');
      hm.appendChild(icon('i-more')); span.appendChild(hm);
      hm.addEventListener('click', function () { hm.focus({ preventScroll: true }); T.colMenu(c, hm); });
      hm.addEventListener('keydown', function (e) { if (e.key === 'ArrowDown') { e.preventDefault(); T.colMenu(c, hm); } });
      var g = mk('div', 'dt-grip'); g.setAttribute('role', 'separator'); g.setAttribute('aria-orientation', 'vertical'); g.tabIndex = 0;
      g.id = 'dt-' + T.id + '-grip-' + c.i; g.setAttribute('aria-label', 'Resize ' + c.label); g.setAttribute('aria-valuemin', colMin(c)); g.setAttribute('aria-valuemax', DT_MAX);
      th.appendChild(g); c.grip = g; drag(c, g);
    });

    function hidden(c) { var m = prefs().columns[T.pk]; return m && typeof m[c.id] === 'boolean' ? !m[c.id] : c.hide; }
    T.layout = function () {
      T.cols.forEach(function (c) {
        var off = c.canHide && hidden(c);
        c.off = off;
        c.th.classList.toggle('dt-off', off);
        T.rows.forEach(function (r) { var td = r.tr.children[c.i + lead]; if (td) td.classList.toggle('dt-off', off); });
      });
      T.applyWidths();
      root.classList.toggle('dt-compact', prefs().density === 'compact');
      var e = $('.dt-empty td', tbody); if (e) e.colSpan = T.cols.filter(function (c) { return !c.off; }).length;
    };

    function rowText(r) {
      if (r.text === null) {
        var parts = [r.tr.getAttribute('data-row') || ''];
        Array.prototype.forEach.call(r.tr.children, function (td, i) {
          var c = T.cols[i - lead]; if (c && (c.pin || c.th.getAttribute('data-sort') === 'false') && !c.canSort) return;
          parts = cellText(td, parts);
        });
        r.text = parts.join(' ').replace(/\s+/g, ' ').toLowerCase();
      }
      return r.text;
    }
    function firstDir(c) { // like TanStack: numbers sort descending first, text ascending; judged by the first defined value
      for (var n = 0; n < T.rows.length; n++) { var v = rowVal(T.rows[n], c.i); if (v !== undefined) return typeof v === 'number' ? 'desc' : 'asc'; }
      return 'asc';
    }

    T.setQuery = function (v) {
      if (v === S.q) return;
      S.q = v; S.page = 0; if (q && q.value !== v) q.value = v; T.render();
    };
    T.setPill = function (k) { if (k === S.pill) return; S.pill = k; S.page = 0; T.render(); };
    T.sortBy = function (c) {
      var s = S.sort, f = firstDir(c);
      if (!s || s.col !== c.id) S.sort = { col: c.id, dir: f };
      else if (s.dir === f) S.sort = { col: c.id, dir: f === 'asc' ? 'desc' : 'asc' };
      else S.sort = null;
      S.page = 0; T.render();
    };
    T.clearAll = function () {
      S.q = ''; S.pill = 'all'; S.page = 0;
      if (q) q.value = ''; if (topbar) topbar.value = '';
      T.render();
      var f = q || topbar; if (f) f.focus({ preventScroll: true });
    };
    T.goto = function (n, which) {
      S.page = n; T.render(true);
      var b = which === 'prev' || which === 'next' ? $('.dt-step[data-step=' + which + ']', pg) : null;
      (b || $('.dt-pg[aria-current]', pg) || pg).focus({ preventScroll: true });
    };
    T.viewMenu = function () {
      var cur = prefs().density === 'compact' ? 'compact' : 'comfortable';
      var items = [
        { t: 'head', label: 'Density' },
        { t: 'radio', group: 'd', label: 'Comfortable', on: cur === 'comfortable', run: function () { setDensity('comfortable'); } },
        { t: 'radio', group: 'd', label: 'Compact', on: cur === 'compact', run: function () { setDensity('compact'); } }
      ];
      var hideable = T.cols.filter(function (c) { return c.canHide; });
      if (hideable.length) {
        items.push({ t: 'sep' }, { t: 'head', label: 'Columns' });
        hideable.forEach(function (c) {
          items.push({ t: 'check', label: c.label, on: !hidden(c), run: function (on) {
            var m = prefs().columns[T.pk] || (prefs().columns[T.pk] = {}); m[c.id] = on; savePrefs(); T.layout(); T.fit();
          } });
        });
      }
      openPop(vb, items);
    };
    function setDensity(d) { prefs().density = d; savePrefs(); dtLive.forEach(function (t) { t.layout(); t.fit(); }); }

    // sticky header sticks to the page only while the table fits; with a horizontal scroller it sticks inside it
    T.fit = function () {
      wrap.classList.toggle('dt-page', !(table.offsetWidth > wrap.clientWidth + 1));
      T.edge();
    };
    // "More columns" edge: shown while content is scrolled off to the right; the right-pinned column gets a shadow
    T.edge = function () {
      var pin = $('thead .dt-pin-end', table); if (pin) box.style.setProperty('--pin-w', pin.offsetWidth + 'px');
      if (wrap.scrollLeft + wrap.clientWidth < wrap.scrollWidth - 2) box.setAttribute('data-edge', ''); else box.removeAttribute('data-edge');
    };

    // column widths: saved ones switch the table to a fixed layout (every column then has a width)
    function savedWidths() { var p = prefs(); return p.widths[T.pk] || (p.widths[T.pk] = {}); }
    T.applyWidths = function () {
      var w = savedWidths(), any = Object.keys(w).length > 0, total = lead * 44;
      var gone = T.cols.map(function (c) { return c.off || getComputedStyle(c.th).display === 'none'; }); // reads first, then writes
      table.classList.toggle('dt-fixed', any);
      T.cols.forEach(function (c, n) {
        var px = any && !c.off && w[c.id] ? w[c.id] : 0;
        c.th.style.width = px ? px + 'px' : ''; c.th.style.minWidth = px ? px + 'px' : c.w ? c.w + 'px' : '';
        if (!gone[n]) total += w[c.id] || c.w || 160; // not columns a breakpoint hides
      });
      table.style.tableLayout = any ? 'fixed' : ''; table.style.width = any ? total + 'px' : ''; table.style.minWidth = any ? '100%' : '';
      T.cols.forEach(function (c) { if (c.grip) c.grip.setAttribute('aria-valuenow', Math.round(c.th.getBoundingClientRect().width)); });
    };
    T.hasWidths = function () { return Object.keys(savedWidths()).length > 0; };
    T.widen = function (c, px, save) { // px undefined resets this column
      var w = savedWidths();
      if (px !== undefined && !Object.keys(w).length) { // first resize: freeze every other column at its measured width
        T.cols.forEach(function (o) { if (o !== c && !o.off && o.th.offsetWidth) w[o.id] = clampW(o.th.getBoundingClientRect().width, colMin(o)); });
      }
      if (px === undefined) delete w[c.id]; else w[c.id] = clampW(px, colMin(c));
      T.applyWidths(); T.fit(); if (save !== false) savePrefs();
    };
    T.resetWidths = function () { delete prefs().widths[T.pk]; savePrefs(); T.applyWidths(); T.fit(); };
    T.fitCol = function (c) { // widest cell on the page (or the header) + cell padding
      var rg = document.createRange(), w = 0, lab = $('.dt-sort', c.th) || $('.dt-th', c.th);
      (T.shown || []).forEach(function (r) {
        var td = r.tr.children[c.i + lead]; if (!td) return;
        var prev = td.style.whiteSpace; td.style.whiteSpace = 'nowrap'; rg.selectNodeContents(td);
        w = Math.max(w, rg.getBoundingClientRect().width); td.style.whiteSpace = prev;
      });
      T.widen(c, Math.max(w, lab ? lab.scrollWidth + (c.canSort ? 28 : 20) : 0) + 24);
    };
    T.colMenu = function (c, btn) {
      var items = [];
      if (c.canSort) items.push(
        { label: 'Sort ascending', run: function () { S.sort = { col: c.id, dir: 'asc' }; S.page = 0; T.render(); } },
        { label: 'Sort descending', run: function () { S.sort = { col: c.id, dir: 'desc' }; S.page = 0; T.render(); } });
      items.push({ label: 'Fit width', run: function () { T.fitCol(c); } });
      if (c.canHide) items.push({ label: 'Hide column', run: function () {
        var m = prefs().columns[T.pk] || (prefs().columns[T.pk] = {}); m[c.id] = false; savePrefs(); T.layout(); T.fit();
      } });
      items.push({ t: 'sep' }, { label: 'Reset widths', run: function () { T.resetWidths(); } });
      openPop(btn, items);
    };

    // selection (data-select): ids live in S.sel, so it survives a live refresh; filtered-out rows drop out at render
    T.cur = -1; // the row cursor and the range anchor live in S (by row id), so a live rebuild keeps them
    T.paintSel = function () {
      var list = T.list || [], n = 0;
      list.forEach(function (r) { if (S.sel[r.id]) n++; });
      T.rows.forEach(function (r) {
        if (select) { r.tr.setAttribute('aria-selected', S.sel[r.id] ? 'true' : 'false'); r.cb.checked = !!S.sel[r.id]; }
        r.tr.classList.toggle('is-focus', r.show && T.shown && T.shown.indexOf(r) === T.cur);
      });
      if (!select) return;
      hcb.checked = n > 0 && n === list.length; hcb.indeterminate = n > 0 && n < list.length;
      hcb.setAttribute('aria-label', 'Select all ' + list.length + ' ' + T.noun);
      if (!n) { if (S.bulk) { S.bulk.remove(); S.bulk = null; } return; }
      if (!S.bulk) { // one bar per table id, kept across live refreshes (a rebuilt bar would drop keyboard focus and a press in flight)
        var bulk = S.bulk = mk('div', 'dt-bulk'), id = T.id, noun = T.noun, now = function () { return dtById(id); };
        bulk.setAttribute('role', 'region'); bulk.setAttribute('aria-label', 'Selected ' + noun); bulk.setAttribute('data-for', id);
        var cn = mk('span'), cb = mk('button'), xb = mk('button'); cn.appendChild(mk('b')); cn.appendChild(document.createTextNode(' selected'));
        cb.type = 'button'; cb.appendChild(icon('i-copy')); cb.appendChild(document.createTextNode('Copy')); cb.addEventListener('click', function () { var t = now(); if (t) t.copy(); });
        xb.type = 'button'; xb.setAttribute('aria-label', 'Clear selection'); xb.appendChild(icon('i-x')); xb.addEventListener('click', function () { var t = now(); if (t) t.clearSel(); });
        bulk.appendChild(cn); bulk.appendChild(cb); bulk.appendChild(xb);
      }
      if (!S.bulk.parentNode) document.body.appendChild(S.bulk);
      $('b', S.bulk).textContent = n;
    };
    T.clearSel = function () { S.sel = {}; T.paintSel(); };
    T.copy = function () { // the visible columns of the selected rows, in table order, as TSV
      var cols = T.cols.filter(function (c) { return !c.off && !c.pin && c.th.getAttribute('data-sort') !== 'false'; });
      var chosen = (T.list || []).filter(function (r) { return S.sel[r.id]; }), flat = function (x) { return x.replace(/[\t\r\n]+/g, ' ').replace(/ +/g, ' ').trim(); };
      var tsv = [cols.map(function (c) { return c.label; })].concat(chosen.map(function (r) {
        return cols.map(function (c) { return flat(cellText(r.tr.children[c.i + lead], []).join(' ')); });
      })).map(function (row) { return row.join('\t'); }).join('\n');
      copyText(tsv, function () { toast('Copied ' + chosen.length + ' ' + singular(chosen.length, T.noun), 'ok'); }, function () { toast('Could not copy: the browser blocked the clipboard', 'error'); });
    };
    function pick(ids, on) { ids.forEach(function (id) { if (on) S.sel[id] = true; else delete S.sel[id]; }); }
    function shownIds() { return (T.shown || []).map(function (r) { return r.id; }); }
    function tick(r, range) {
      if (range && S.anchor) pick(rangeIds(shownIds(), S.anchor, r.id), true); else pick([r.id], !S.sel[r.id]);
      S.anchor = r.id; T.paintSel();
    }
    if (select) hcb.addEventListener('click', function () {
      var list = T.list || [], all = list.length > 0 && list.every(function (r) { return S.sel[r.id]; });
      S.sel = {}; if (!all) pick(list.map(function (r) { return r.id; }), true);
      T.paintSel();
    });
    tbody.addEventListener('click', function (e) {
      var tr = e.target.closest ? e.target.closest('tr') : null, r = tr && tr._r; if (!r) return;
      if (e.target.closest('.dt-tick input')) { tick(r, e.shiftKey); return; }
      if (e.target.closest('a,button,input,select,textarea,label,summary,form,.dt-tick')) return;
      var h = tr.getAttribute('data-href');
      if (!h || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button) return;
      if (window.getSelection && String(getSelection())) return; // the user was selecting text
      if (sameHref(h)) { skelStart(h); location.href = h; }
    });
    // keyboard: the focused region moves a row cursor
    function move(to, extend) {
      var rows = T.shown || [], n = Math.max(0, Math.min(rows.length - 1, to)), r = rows[n]; if (!r) return;
      T.cur = n; S.cur = r.id; S.ci = n; live.textContent = 'Row ' + (n + 1) + ' of ' + rows.length + ': ' + r.label;
      if (extend && select) { pick(rangeIds(shownIds(), S.anchor || r.id, r.id), true); if (!S.anchor) S.anchor = r.id; }
      T.paintSel(); r.tr.scrollIntoView({ block: 'nearest' });
    }
    wrap.addEventListener('keydown', function (e) {
      if (e.target !== wrap || e.ctrlKey || e.metaKey || e.altKey) return;
      var rows = T.shown || [], at = T.cur < 0 ? -1 : Math.min(T.cur, rows.length - 1), r = at < 0 ? null : rows[at], k = e.key;
      if (k === 'ArrowDown') move(at < 0 ? 0 : at + 1, e.shiftKey);
      else if (k === 'ArrowUp') move(at < 0 ? 0 : at - 1, e.shiftKey);
      else if (k === 'Home') move(0, e.shiftKey);
      else if (k === 'End') move(rows.length - 1, e.shiftKey);
      else if (k === ' ' && r && select) tick(r, false);
      else if (k === 'Enter' && r && sameHref(r.tr.getAttribute('data-href'))) { skelStart(r.tr.getAttribute('data-href')); location.href = r.tr.getAttribute('data-href'); }
      else if (k === 'Escape' && Object.keys(S.sel).length) T.clearSel();
      else return;
      e.preventDefault();
    });


      T.render = function (announce) {
      var qq = S.q.trim().toLowerCase(), list = T.rows.filter(function (r) {
        return (S.pill === 'all' || r.k.indexOf(S.pill) >= 0) && (!qq || rowText(r).indexOf(qq) >= 0);
      });
      var sc = S.sort && colById[S.sort.col] && colById[S.sort.col].canSort ? colById[S.sort.col] : null;
      if (!sc) S.sort = null;
      if (sc) {
        var sign = S.sort.dir === 'desc' ? -1 : 1;
        list.sort(function (a, b) {
          var x = rowVal(a, sc.i), y = rowVal(b, sc.i);
          if (x === undefined || y === undefined) return x === y ? a.idx - b.idx : x === undefined ? 1 : -1; // empty last, either way
          return sign * cmpVal(x, y) || a.idx - b.idx;
        });
      }
      T.list = list;
      var matched = list.length, size = mode === 'client' ? prefs().size : Math.max(matched, 1);
      var pages = Math.max(1, Math.ceil(matched / size));
      S.page = Math.max(0, Math.min(S.page, pages - 1));
      T.rows.forEach(function (r) { r.show = false; });
      T.shown = list.slice(S.page * size, S.page * size + size); T.shown.forEach(function (r) { r.show = true; });
      var keep = {}; list.forEach(function (r) { if (S.sel[r.id]) keep[r.id] = true; }); S.sel = keep; // a filter drops hidden rows from the selection
      var inList = {}; list.forEach(function (r) { inList[r.idx] = 1; });
      var order = list.concat(T.rows.filter(function (r) { return !inList[r.idx]; }));
      var cur = Array.prototype.slice.call(tbody.children), moved = order.some(function (r, n) { return cur[n] !== r.tr; });
      if (moved) order.forEach(function (r) { tbody.appendChild(r.tr); });
      T.rows.forEach(function (r) { r.tr.hidden = !r.show; });
      var none = $('.dt-empty', tbody);
      if (!list.length) {
        if (!none) {
          none = mk('tr', 'dt-empty'); var td = mk('td'), p = mk('span'), cb = mk('button', 'dt-link');
          p.className = 'dt-empty-t'; cb.type = 'button'; cb.id = 'dt-' + T.id + '-clear-empty'; cb.textContent = 'Clear filters'; cb.addEventListener('click', function () { T.clearAll(); });
          td.appendChild(p); td.appendChild(cb); none.appendChild(td); tbody.appendChild(none);
        }
        $('.dt-empty-t', none).textContent = emptyText + ' ';
        $('button', none).hidden = !(S.q.trim() || S.pill !== 'all');
        none.firstChild.colSpan = T.cols.filter(function (c) { return !c.off; }).length;
      } else if (none) none.remove();

      // headers
      T.cols.forEach(function (c) {
        if (!c.canSort) return;
        var d = sc === c ? S.sort.dir : 'none';
        c.th.setAttribute('aria-sort', d === 'none' ? 'none' : d === 'asc' ? 'ascending' : 'descending');
        if (c.dir !== d) { // fade the arrow when the direction changes (not on the first render)
          $('use', c.arrow).setAttribute('href', '#' + ARROW[d]);
          if (c.dir) restartAnim(c.arrow, 'fade');
          c.dir = d;
        }
        c.btn.classList.toggle('on', d !== 'none');
      });
      // pills
      pillBtns.forEach(function (b) { b.setAttribute('aria-pressed', b._key === S.pill ? 'true' : 'false'); });
      if (more) setSel(more, spec.some(function (p) { return p.key === S.pill && pillBtns.every(function (b) { return b._key !== p.key; }); }) ? S.pill : '');
      // summary
      var parts = [], act = spec.filter(function (p) { return p.key === S.pill; })[0];
      if (act && S.pill !== 'all') parts.push(act.label.toLowerCase());
      if (S.q.trim()) parts.push('matching “' + S.q.trim() + '”');
      var den = S.pill === 'all' || !act ? T.rows.length : act.count;
      sumT.textContent = filterSummary(matched, den, T.noun, parts) + ' ';
      clear.hidden = !(S.q.trim() || S.pill !== 'all');
      // pager
      var Noun = T.noun.charAt(0).toUpperCase() + T.noun.slice(1), at = function (i) { return (i * size + 1) + '–' + Math.min(matched, (i + 1) * size); };
      var showPager = mode === 'client' && matched > DT_SIZES[0];
      foot.hidden = !showPager;
      if (!showPager) pg.innerHTML = '';
      if (showPager) {
        cnt.innerHTML = ''; cnt.appendChild(document.createTextNode(Noun + ' '));
        [at(S.page), ' of ', String(matched)].forEach(function (t, n) { if (n === 1) cnt.appendChild(document.createTextNode(t)); else { var b = mk('b'); b.textContent = t; cnt.appendChild(b); } });
        setSel(sizeSel, String(size));
        pg.innerHTML = '';
        if (pages > 1) {
          var step = function (to, label, way) {
            var b = mk('button', 'dt-pg dt-step'); b.type = 'button'; b.id = 'dt-' + T.id + '-' + way; b.setAttribute('aria-label', label); b.setAttribute('data-step', way);
            b.setAttribute('aria-disabled', to < 0 || to >= pages ? 'true' : 'false');
            var ic = icon('i-chevron-right'); ic.setAttribute('class', 'ic dt-chev ' + way); b.appendChild(ic);
            b.addEventListener('click', function () { if (to >= 0 && to < pages) T.goto(to, way); });
            return b;
          };
          var list2 = function (arr) {
            var s = mk('span', 'dt-plist');
            arr.forEach(function (n) {
              if (n === 'gap') { var g = mk('span', 'dt-gap'); g.textContent = '…'; g.setAttribute('aria-hidden', 'true'); s.appendChild(g); return; }
              var b = mk('button', 'dt-pg'); b.type = 'button'; b.id = 'dt-' + T.id + '-pg-' + n; b.textContent = n + 1; b.setAttribute('aria-label', 'Page ' + (n + 1));
              if (n === S.page) b.setAttribute('aria-current', 'page');
              b.addEventListener('click', function () { if (n !== S.page) T.goto(n, n); });
              s.appendChild(b);
            });
            return s;
          };
          var narrow = window.matchMedia && matchMedia('(max-width:639px)').matches;
          pg.appendChild(step(S.page - 1, 'Previous page', 'prev'));
          pg.appendChild(list2(pageList(S.page, pages, narrow ? 5 : 7)));
          pg.appendChild(step(S.page + 1, 'Next page', 'next'));
        }
      }
      if (announce) {
        live.textContent = Noun + ' ' + at(S.page) + ' of ' + matched + ' · page ' + (S.page + 1) + ' of ' + pages;
        var top = wrap.getBoundingClientRect().top;
        if (top < 0 || top > window.innerHeight) wrap.scrollIntoView({ block: 'start' });
      }
      // re-derive the cursor from its row id (after a rebuild or a re-sort); a vanished row clamps to the nearest index
      T.cur = -1;
      if (S.cur != null && T.shown.length) {
        for (var ci = 0; ci < T.shown.length; ci++) if (T.shown[ci].id === S.cur) T.cur = ci;
        if (T.cur < 0) T.cur = Math.min(S.ci || 0, T.shown.length - 1); // filtered out: show the nearest row but keep S.cur, so clearing the filter returns to the original row
      }
      T.paintSel();
      T.fit();
    };

    if (sizeSel) setSel(sizeSel, String(prefs().size));
    dtLive = dtLive.filter(function (t) { return document.contains(t.root); });
    dtLive.push(T);
    T.layout(); T.render();
    return T;
  }
  function wireTables(root) {
    $$('.dt', root).forEach(function (el) { if (!el._dt) el._dt = buildTable(el); });
  }
  var dtNarrow = window.matchMedia ? matchMedia('(max-width:639px)').matches : null;
  var dtRaf = 0;
  window.addEventListener('resize', function () {
    if (dtRaf) return;
    dtRaf = requestAnimationFrame(function () {
      dtRaf = 0;
      var n = window.matchMedia && matchMedia('(max-width:639px)').matches; // the page list has 5 slots when narrow, 7 otherwise
      dtLive.forEach(function (t) { if (t.hasWidths()) t.applyWidths(); }); // a breakpoint may hide or show columns of a fixed layout
      dtLive.forEach(function (t) { if (dtNarrow !== null && n !== dtNarrow) t.render(); else t.fit(); });
      dtNarrow = n;
    });
  });

  // ---- search filter ----
  // The topbar box is the search input of the page's tables (overview, monitors); each table filters itself.
  function wireSearch(root) {
    var s = $('input[data-search]', root); if (!s || s._wired) return; s._wired = true;
    s.addEventListener('input', function () { dtLive.forEach(function (t) { if (t.topbar === s) t.setQuery(s.value); }); });
  }

  // ---- inline validation ----
  // Capture-phase submit listener, so it runs before the confirm/async/panel handlers. novalidate is added
  // by JS only: without JS the browser (and then the server) validates as before.
  var CONSTRAINTS = '[required],[minlength],[maxlength],[pattern],[min],[max],[type=email],[type=url],[data-match]';
  var vid = 0;
  function validatable(f) {
    return Array.prototype.slice.call(f.elements).filter(function (el) {
      return el.name && el.willValidate && !el.disabled && el.type !== 'hidden';
    });
  }
  function fieldMessage(el) {
    var v = el.validity, val = el.value, type = (el.getAttribute('type') || '').toLowerCase();
    if (v.valueMissing) return 'Required.';
    if (v.typeMismatch && type === 'email') return 'Enter a full email address, like name@example.com.';
    if (v.typeMismatch && type === 'url') return 'Enter a full URL that starts with http:// or https://.';
    // measured by hand: the browser's tooShort only fires after a user edit, not for filled or prefilled values
    if (val && el.minLength > 0 && val.length < el.minLength) return 'Use at least ' + el.minLength + ' characters.';
    if (val && el._touched && el.maxLength >= 0 && val.length > el.maxLength) return 'Use at most ' + el.maxLength + ' characters.';
    if (v.patternMismatch) return el.getAttribute('data-msg') || el.title || 'Use the format shown.';
    if (v.rangeUnderflow) return 'Must be at least ' + el.min + '.';
    if (v.rangeOverflow) return 'Must be at most ' + el.max + '.';
    if (v.badInput) return 'Enter a valid value.';
    if (!v.valid) return el.validationMessage || 'Invalid value.';
    var m = el.getAttribute('data-match');
    if (m && el.form && el.form.elements[m] && el.form.elements[m].value !== val) return "Doesn't match the field above.";
    return '';
  }
  function setFieldMsg(el, msg) {
    if (!el.id && !el._mid) el._mid = 'f' + (++vid);
    var id = (el.id || el._mid) + '-msg', p = document.getElementById(id);
    var ids = (el.getAttribute('aria-describedby') || '').split(/\s+/).filter(function (x) { return x && x !== id; });
    if (!msg) {
      if (p) p.remove();
      el.removeAttribute('aria-invalid');
      if (ids.length) el.setAttribute('aria-describedby', ids.join(' ')); else el.removeAttribute('aria-describedby');
      return;
    }
    if (!p) {
      p = document.createElement('p'); p.className = 'field-msg'; p.id = id;
      var anchor = el, field = el.closest('.field');
      while (field && anchor.parentNode !== field) anchor = anchor.parentNode;
      anchor.parentNode.insertBefore(p, anchor.nextSibling);
    }
    p.textContent = ''; p.appendChild(icon('i-alert'));
    var t = document.createElement('span'); t.textContent = msg; p.appendChild(t);
    el.setAttribute('aria-invalid', 'true');
    ids.push(id); el.setAttribute('aria-describedby', ids.join(' '));
  }
  function checkField(el) { var m = fieldMessage(el); setFieldMsg(el, m); return !m; }
  function wireValidation(root) {
    $$('form', root).forEach(function (f) {
      if (f._vw) return;
      if (!f.querySelector(CONSTRAINTS)) return;
      f._vw = true; f.setAttribute('novalidate', '');
    });
  }
  document.addEventListener('submit', function (e) {
    var f = e.target;
    if (!f || !f._vw || (e.submitter && e.submitter.formNoValidate)) return;
    var bad = validatable(f).filter(function (el) { return !checkField(el); });
    if (!bad.length) return;
    e.preventDefault(); e.stopImmediatePropagation();
    f._failed = true;
    (bad[0]._ddBtn || bad[0]).focus();
  }, true);
  document.addEventListener('input', function (e) {
    var el = e.target, f = el && el.form; if (!f || !f._vw) return;
    el._touched = true;
    if (f._failed) validatable(f).forEach(function (x) {
      if (x === el || x.hasAttribute('aria-invalid') || (x._touched && x.getAttribute('data-match') === el.name)) checkField(x);
    });
  }, true);
  document.addEventListener('focusout', function (e) {
    var el = e.target, f = el && el.form; if (!f || !f._vw || !el.willValidate || el.type === 'hidden') return;
    // moving to a submit button: skip, or the message pushes the button away mid-click and the click is lost
    var to = e.relatedTarget; if (to && to.form === f && to.type === 'submit') return;
    if (el._touched || f._failed) checkField(el);
  }, true);
  document.addEventListener('reset', function (e) {
    var f = e.target; if (!f || !f._vw) return;
    f._failed = false;
    setTimeout(function () { validatable(f).forEach(function (el) { el._touched = false; setFieldMsg(el, ''); }); }, 0);
  }, true);

  // ---- live refresh over SSE ----
  var refreshing = false, pending = false, timer = null, pressing = false, pressTimer = null;
  // A refresh between pointerdown and click would replace the control under the pointer and lose the click, so hold
  // while a press inside #content is in flight; the release reschedules a held refresh (pending is never left parked).
  document.addEventListener('pointerdown', function (e) {
    if (!(e.target.closest && e.target.closest('#content, .dt-bulk'))) return;
    pressing = true; clearTimeout(pressTimer); pressTimer = setTimeout(releasePress, 5000); // safety net if the release is lost
  }, true);
  function releasePress() {
    clearTimeout(pressTimer);
    setTimeout(function () { pressing = false; if (pending && !refreshing) { pending = false; schedule(); } }, 0);
  }
  ['pointerup', 'pointercancel', 'click'].forEach(function (t) { document.addEventListener(t, function () { if (pressing) releasePress(); }, true); });
  function editing() {
    var a = document.activeElement;
    if (a && /^(INPUT|TEXTAREA|SELECT)$/.test(a.tagName) && a.form && !a.hasAttribute('data-search')) return true;
    if ($('#content[data-no-refresh]')) return true;
    if ($('#content [aria-invalid=true]')) return true;            // client-side field errors are showing
    if (skel.saved) return true;                                   // a skeleton is showing; a swap would drop the saved page
    if (pressing) return true;                                     // a press is in flight; the click must land on the control it started on
    if (dtDragging) return true;                                   // a column is being resized
    if ($('.dd-menu')) return true;                                // a dropdown is open; replacing it would drop the menu
    var dlg = $('#confirm'); if (dlg && dlg.open) return true;     // a confirm is pending; replacing the form would detach it
    return $$('#content form').some(function (f) { return f._dirty; }); // unsaved edits
  }
  // Identity of a focusable element across a re-render: id, else href, else form action + button text, else name, else data-action + text.
  // An element with none of these is never restored, so focus cannot land on a different row's control.
  function focusKey(el) {
    if (el._sel) { var sk = el._sel.name || el._sel.id; return sk ? 'sel:' + sk : ''; } // a dropdown button: its native select is the identity (the button id is regenerated)
    if (el.id) return 'id:' + el.id;
    var h = el.getAttribute('href'); if (h) return 'href:' + h;
    var f = el.form || (el.closest && el.closest('form'));
    if (f && f.getAttribute('action')) { // the same action + label repeats per row (invite Revoke, per-channel test), so add the button value and the form's id field
      var idf = f.querySelector('[name=id]');
      return 'form:' + f.getAttribute('action') + '|' + el.textContent.trim() + '|' + (el.name || '') + '|' + (el.value || '') + '|' + (idf ? idf.value : '');
    }
    if (el.name) return 'name:' + el.name;
    var da = el.getAttribute('data-action'); if (da) return 'act:' + da + '|' + el.textContent.trim();
    return '';
  }
  // Row keys (table + data-id) with their data-k state, so a refresh can flash only the rows whose state changed.
  function rowStates(root) {
    var m = {};
    $$('.dt[data-table] tr[data-id][data-k]', root).forEach(function (r) { m[r.closest('.dt').getAttribute('data-table') + ':' + r.getAttribute('data-id')] = r.getAttribute('data-k'); });
    return m;
  }
  function flashChanged(root, before) {
    if (reduced()) return;
    $$('.dt[data-table] tr[data-id][data-k]', root).forEach(function (r) {
      var k = before[r.closest('.dt').getAttribute('data-table') + ':' + r.getAttribute('data-id')];
      if (k !== undefined && k !== r.getAttribute('data-k')) r.classList.add('row-flash');
    });
  }
  window.refresh = function () {
    if (document.body.classList.contains('panel-open') || editing()) { pending = true; return; }
    if (refreshing) { pending = true; return; }
    refreshing = true;
    fetch(location.href, { headers: { 'X-Partial': '1' }, credentials: 'same-origin' })
      .then(function (r) {
        if (r.status === 401 || r.redirected) { sessionExpired(); return null; }
        return r.ok ? r.text() : null;
      })
      .then(function (html) {
        if (html === null) return;
        if (editing()) { pending = true; return; } // a dialog or edit began while this was in flight
        var main = $('#content'); if (!main) return;
        var q = $('input[data-search]'); var qv = q ? q.value : '';
        var FOC = 'button,a[href],input,select,textarea,summary,.dt-wrap,.dt-grip', act = document.activeElement;
        var fk = act && main.contains(act) ? focusKey(act) : '', ftag = fk ? act.tagName : '';
        var dq = act && act.classList && act.classList.contains('dt-q') ? { id: act.closest('.dt').getAttribute('data-table'), a: act.selectionStart, b: act.selectionEnd } : null;
        var before = rowStates(main);
        main.innerHTML = html;
        wire(main); dropBulk(); flashChanged(main, before);
        if (fk && document.activeElement !== act) { // keep the keyboard position across a refresh, but only on the same element
          var m = $$(FOC, main).filter(function (x) { return x.tagName === ftag && focusKey(x) === fk; });
          if (m.length === 1) m[0].focus({ preventScroll: true }); // an ambiguous key restores nothing
        }
        if (dq) { // the in-table search box has no identity key of its own; find it by its table
          var nb = $('.dt[data-table="' + dq.id + '"] .dt-q', main);
          if (nb) { nb.focus({ preventScroll: true }); try { nb.setSelectionRange(dq.a, dq.b); } catch (e) { } }
        }
        if (qv) { var nq = $('input[data-search]'); if (nq) { nq.value = qv; nq.dispatchEvent(new Event('input')); } }
        markUpdated(); dotState(); beat();
      })
      .catch(function () { })
      .then(function () { refreshing = false; if (pending) { pending = false; schedule(); } });
  };
  function schedule() { clearTimeout(timer); timer = setTimeout(refresh, 250); }
  // The live dot rests; it pulses a few times (CSS, finite) after each update or reconnect, then stops (WCAG 2.2.2).
  // It is looked up each time because a refresh replaces #content, and with it the dot.
  var liveOff = false;
  function dotState() { var d = $('.live'); if (d) d.classList.toggle('off', liveOff); }
  // One short pulse (about 5s), at most once a minute; a refresh that swaps the dot mid-pulse carries it over.
  var lastBeat = -Infinity;
  function beat() {
    var d = $('.live'), now = window.performance ? performance.now() : Date.now(); if (!d || liveOff) return;
    if (now - lastBeat < 4500) { d.classList.add('beat'); return; }
    if (now - lastBeat < 60000) return;
    lastBeat = now; d.classList.add('beat');
  }
  document.addEventListener('animationend', function (e) {
    if (e.target.classList && e.target.classList.contains('live')) e.target.classList.remove('beat');
    if (e.animationName === 'row-flash') { var tr = e.target.closest('tr'); if (tr) tr.classList.remove('row-flash'); }
  });
  // Infinite animations (the Check spinner) stop while the tab is hidden.
  function syncPaused() { document.documentElement.classList.toggle('paused', document.hidden); }
  document.addEventListener('visibilitychange', syncPaused); syncPaused();
  function connect() {
    if (!window.EventSource || !document.body.hasAttribute('data-live')) return;
    var es = new EventSource('/events');
    es.onopen = function () { liveOff = false; dotState(); beat(); schedule(); }; // catch up on anything missed while disconnected
    es.onmessage = function () { beat(); schedule(); };
    es.onerror = function () { liveOff = true; dotState(); };
  }

  function copyField(btn) {
    var inp = $(btn.getAttribute('data-copy-target')); if (!inp) return;
    var done = function () {
      toast('Link copied', 'ok');
      var cur = $('.ic:not(.swap-out)', btn); // clipboard-check for about 1.5s, then back
      if (!btn._copied && cur) { btn._copied = true; swapIcon(cur, 'i-copied', 150); }
      clearTimeout(btn._ct);
      btn._ct = setTimeout(function () { var c2 = $('.ic:not(.swap-out)', btn); btn._copied = false; if (c2) swapIcon(c2, 'i-copy', 150); }, 1500);
    };
    var fallback = function () {
      inp.focus(); inp.select();
      var ok = false; try { ok = document.execCommand('copy'); } catch (e) { }
      if (ok) done(); else toast('Press Ctrl+C to copy the link', 'error');
    };
    if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(inp.value).then(done, fallback); else fallback();
  }

  // ---- declarative actions (no inline handlers, so the CSP can forbid inline script) ----
  document.addEventListener('click', function (e) {
    var el = e.target.closest ? e.target.closest('[data-action]') : null; if (!el) return;
    var a = el.getAttribute('data-action');
    if (a === 'open-panel') openPanel();
    else if (a === 'close-panel') closePanel();
    else if (a === 'theme') cycleTheme();
    else if (a === 'copy') copyField(el);
  });
  document.addEventListener('change', function (e) {
    if (e.target.hasAttribute && e.target.hasAttribute('data-autosubmit') && e.target.form) e.target.form.submit();
  });

  // ---- loading skeletons ----
  // Pages are server-rendered, so a navigation shows nothing new until the response arrives. If it takes
  // longer than 250ms, #content is swapped for a skeleton shaped like the destination (the <template>s in
  // layout.html). The real nodes are kept, not copied, and put back if the page is restored from the
  // back/forward cache or the navigation never unloads this page (Stop, a download). Live refresh never shows one.
  var skel = { timer: null, safety: null, saved: null, focus: null, y: 0, nav: null };
  var SKEL_SKIP = /^\/(static|api|auth|events|healthz|logout)(\/|$)/;
  function skelKind(u) {
    var p = u.pathname;
    if (p === '/') return 'overview';
    if (p === '/targets') return 'table';
    if (/^\/targets\/\d+$/.test(p)) { var t = new URLSearchParams(u.search).get('tab'); return !t || t === 'overview' ? 'detail' : 'generic'; }
    return p === '/incidents' ? 'list' : 'generic';
  }
  function skelStart(href) {
    var u; try { u = new URL(href, location.href); } catch (e) { return; }
    clearTimeout(skel.timer);
    if (u.origin !== location.origin || SKEL_SKIP.test(u.pathname)) return;
    skel.timer = setTimeout(function () { skelShow(u); }, 250);
  }
  function skelShow(u) {
    var c = $('#content'), t = $('#skel-' + skelKind(u));
    if (!c || !t || skel.saved) return;
    var f = document.createDocumentFragment(), a = document.activeElement;
    skel.focus = a && c.contains(a) ? a : null;
    skel.y = window.pageYOffset;
    while (c.firstChild) f.appendChild(c.firstChild);
    skel.saved = f;
    c.appendChild(t.content.cloneNode(true));
    window.scrollTo(0, 0);
    // the destination's sidebar item takes the highlight until the page arrives
    var cur = $('.side a.nav.active'), dest = $('.side a.nav[href="' + (/^\/targets\//.test(u.pathname) ? '/targets' : /^\/settings\//.test(u.pathname) ? '/settings/profile' : u.pathname) + '"]');
    if (dest && dest !== cur) {
      skel.nav = { cur: cur, dest: dest };
      if (cur) { cur.classList.remove('active'); cur.removeAttribute('aria-current'); }
      dest.classList.add('active'); dest.setAttribute('aria-current', 'page');
    }
    try { sessionStorage.setItem('skel-shown', '1'); } catch (e) { } // the next page skips its enter fade
    skel.safety = setTimeout(skelHide, 10000);
  }
  function skelHide() {
    clearTimeout(skel.timer); clearTimeout(skel.safety);
    var c = $('#content'), f = skel.saved; skel.saved = null;
    if (!f || !c) return;
    while (c.firstChild) c.removeChild(c.firstChild);
    c.appendChild(f);
    if (skel.nav) {
      skel.nav.dest.classList.remove('active'); skel.nav.dest.removeAttribute('aria-current');
      if (skel.nav.cur) { skel.nav.cur.classList.add('active'); skel.nav.cur.setAttribute('aria-current', 'page'); }
      skel.nav = null;
    }
    try { sessionStorage.removeItem('skel-shown'); } catch (e) { }
    window.scrollTo(0, skel.y);
    if (skel.focus) { skel.focus.focus({ preventScroll: true }); skel.focus = null; }
    if (pending && !refreshing) { pending = false; schedule(); }
  }
  document.addEventListener('click', function (e) {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    var a = e.target.closest ? e.target.closest('a[href]') : null;
    if (!a || (a.target && a.target !== '_self') || a.hasAttribute('download') || a.hasAttribute('data-no-skel')) return;
    var h = a.getAttribute('href');
    if (h.charAt(0) === '#' || (a.origin === location.origin && a.pathname === location.pathname && a.search === location.search && a.hash)) return;
    skelStart(a.href);
  });
  window.addEventListener('pageshow', function (e) { if (e.persisted) skelHide(); });

  // ---- "updated" footer ----
  function markUpdated() { var u = $('[data-updated]'); if (u) { u._t = Date.now(); u.textContent = 'Updated just now'; } }
  function tickUpdated() {
    var u = $('[data-updated]'); if (!u || !u._t) return;
    var s = Math.round((Date.now() - u._t) / 1000);
    u.textContent = s < 5 ? 'Updated just now' : s < 60 ? 'Updated ' + s + 's ago' : 'Updated ' + Math.round(s / 60) + 'm ago';
  }

  // ---- shortcuts ----
  document.addEventListener('keydown', function (e) {
    var t = e.target;
    if (e.key === 'Tab' && document.body.classList.contains('panel-open') && !(t && t.closest && t.closest('dialog'))) { panelTab(e); return; }
    if (t && t.closest && t.closest('.dd, .dd-menu')) { if (e.key === 'Escape') closePanel(); return; } // dropdowns own their keys
    if (t && t.closest && t.closest('dialog')) return;
    if (t && t.classList && t.classList.contains('dt-wrap') && /^(Arrow(Up|Down|Left|Right)|Home|End| |Enter|Escape)$/.test(e.key)) return; // a table region owns only its own keys; / n t still work
    if (t && /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName)) { if (e.key === 'Escape') { e.target.blur(); closePanel(); } return; }
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.key === 'Escape') closePanel();
    if (document.body.classList.contains('panel-open')) return; // modal: no page shortcuts behind it
    if (e.key === 'n' && $('#panel')) { e.preventDefault(); openPanel(); }
    if (e.key === '/') { var s = $('input[data-search]') || $('.dt-q'); if (s) { e.preventDefault(); s.focus(); } }
    if (e.key === 't') cycleTheme();
  });

  function wireDirty(root) {
    $$('form', root).forEach(function (f) {
      if (f._dw) return; f._dw = true;
      f.addEventListener('input', function (e) { if (!e.target.hasAttribute('data-search')) f._dirty = true; });
    });
  }
  // Tells the server the browser's time zone when it differs from the one the server has (body[data-tz]; empty
  // means none yet). The server says whether the user's effective zone changed; only then is the page reloaded.
  // A report the server rejected as not a zone (422) is not repeated for the rest of the tab session.
  function detectTZ() {
    var b = document.body, tz = '';
    if (!b.hasAttribute('data-tz') || b.hasAttribute('data-tz-skip')) return; // skip: this page answered a POST, a reload would repeat it
    try { tz = Intl.DateTimeFormat().resolvedOptions().timeZone || ''; } catch (e) { }
    if (!tz || tz === b.getAttribute('data-tz')) return;
    try { if (sessionStorage.getItem('tz-rejected') === tz) return; } catch (e) { }
    var fd = new FormData(); fd.append('tz', tz);
    fetch('/settings/tz-detected', { method: 'POST', body: fd, headers: { 'Accept': 'application/json' }, credentials: 'same-origin' })
      .then(function (r) {
        if (r.ok) return r.json();
        if (r.status === 422) { try { sessionStorage.setItem('tz-rejected', tz); } catch (e) { } } // 429 and others may pass later
        return null;
      })
      .then(function (j) { if (j && j.changed) location.replace(location.href); })
      .catch(function () { });
  }
  function wire(root) { wireValidation(root); wireTables(root); wireDropdowns(root); wireConfirms(root); wireAsync(root); wireSearch(root); wireDirty(root); }
  document.addEventListener('DOMContentLoaded', function () {
    try { if (sessionStorage.getItem('skel-shown')) { sessionStorage.removeItem('skel-shown'); var ec = $('#content'); if (ec) ec.classList.add('no-enter'); } } catch (e) { }
    toastRegions(); liftFlash(); wire(document); wirePanel(); connect(); detectTZ();
    markUpdated();
    setInterval(tickUpdated, 5000);
  });
})();
