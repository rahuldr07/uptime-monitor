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
  };
  window.cycleTheme = function () {
    var cur = document.documentElement.getAttribute('data-theme') || 'auto';
    setTheme(cur === 'auto' ? 'dark' : cur === 'dark' ? 'light' : 'auto');
  };

  // ---- toasts ----
  // Live regions are created once at load (a region only announces what is inserted after it exists):
  // polite for ok/info, assertive for errors. Dismissal is the end of the CSS countdown on .toast-bar,
  // so pausing that animation (hover/focus) pauses dismissal. Errors have no bar and stay until closed.
  var toastN = 0, MAX_TOASTS = 4;
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
    t._leaving = true; t.classList.add('leaving');
    setTimeout(function () { t.remove(); }, reduced() ? 0 : 120);
  }
  function restartAnim(el, cls) {
    if (cls) { el.classList.remove(cls); void el.offsetWidth; el.classList.add(cls); return; }
    el.style.animation = 'none'; void el.offsetWidth; el.style.animation = '';
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
    t.className = 'toast' + (kind ? ' ' + kind : ''); t._msg = msg; t._kind = kind; t._n = ++toastN;
    t.appendChild(icon(kind === 'ok' ? 'i-check' : kind === 'error' ? 'i-alert' : 'i-info'));
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
        $('use', head).setAttribute('href', danger ? '#i-alert' : '#i-info');
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
            if (j.message) toast(j.message, j.ok === false ? 'error' : 'ok');
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
  function clearResult(r) { r.className = 'alert'; r.hidden = true; r.textContent = ''; }
  // kind: ok | error | info; title is shown bold above detail when both are given
  function showResult(r, kind, title, detail) {
    r.className = 'alert ' + kind; r.textContent = '';
    var d = document.createElement('div');
    if (detail) { var b = document.createElement('strong'); b.textContent = title; d.appendChild(b); d.appendChild(document.createTextNode(detail)); }
    else d.textContent = title;
    r.appendChild(icon(kind === 'ok' ? 'i-check' : kind === 'error' ? 'i-alert' : 'i-info')); r.appendChild(d);
    r.hidden = false;
  }
  function wirePanel() {
    var f = $('#panel form'); if (!f || f._wired) return; f._wired = true;
    var url = f.querySelector('input[name=url]'), name = f.querySelector('input[name=name]');
    var res = function () { return f.querySelector('[data-test-result]'); };
    url.addEventListener('blur', function () {
      if (name.value.trim() || !url.value.trim()) return;
      try { var h = new URL(url.value.trim()).hostname.replace(/^www\./, ''); name.value = h.charAt(0).toUpperCase() + h.slice(1); } catch (e) { }
    });
    var tb = f.querySelector('#test-btn'); if (tb) tb.addEventListener('click', function () {
      var r = res(); showResult(r, 'info', 'Testing…');
      fetch('/targets/test', { method: 'POST', body: new FormData(f), headers: { 'Accept': 'application/json' }, credentials: 'same-origin' })
        .then(function (x) { if (x.status === 401) { sessionExpired(); throw 0; } return x.json(); })
        .then(function (j) {
          if (j.error && !j.status_code) { showResult(r, 'error', j.error); return; }
          showResult(r, j.ok ? 'ok' : 'error', j.ok ? 'Test passed' : 'Test failed',
            (j.status_code ? 'HTTP ' + j.status_code + ' ' : '') + 'in ' + j.latency_ms + ' ms' + (j.error ? ' · ' + j.error : ''));
        })
        .catch(function () { showResult(r, 'error', 'Could not run the test'); });
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
    d.val.textContent = o ? o.text : '';
    d.btn.disabled = d.s.disabled;
  }
  function optText(d, i) { return d.s.options[i].text.trim().toLowerCase(); }
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
    var r = d.btn.getBoundingClientRect(), m = d.menu, vw = window.innerWidth, vh = window.innerHeight;
    m.style.maxHeight = '280px'; m.style.top = '0'; m.style.bottom = 'auto'; m.style.left = '0'; m.style.minWidth = r.width + 'px';
    var h = m.offsetHeight, w = m.offsetWidth, below = vh - r.bottom - 8, above = r.top - 8;
    if (h > below && above > below) { // flip upward, anchored at the button so filtering can shrink it in place
      m.style.top = 'auto'; m.style.bottom = (vh - r.top + 4) + 'px'; m.style.maxHeight = Math.min(280, above - 4) + 'px';
    } else { m.style.top = (r.bottom + 4) + 'px'; m.style.maxHeight = Math.min(280, Math.max(below - 4, 80)) + 'px'; }
    m.style.left = Math.max(8, Math.min(r.left, vw - 8 - w)) + 'px';
  }
  function applyFilter(d) {
    var q = d.filter.value.trim().toLowerCase(), any = false;
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
    d.menu = menu; d.list = list; d.open = true; openDD = d;
    d.wrap.classList.add('open'); d.btn.setAttribute('aria-expanded', 'true'); d.btn.setAttribute('aria-controls', list.id);
    menu.addEventListener('mousedown', function (e) { if (e.target !== d.filter) e.preventDefault(); }); // keep focus where it is
    menu.addEventListener('click', function (e) { var li = e.target.closest('.dd-opt'); if (li && li.getAttribute('aria-disabled') !== 'true') commit(d, +li.getAttribute('data-i')); });
    menu.addEventListener('mouseover', function (e) { var li = e.target.closest('.dd-opt'); if (li && li.getAttribute('aria-disabled') !== 'true') setHi(d, +li.getAttribute('data-i'), false); });
    menu.addEventListener('keydown', function (e) { ddKey(d, e); });
    menu.addEventListener('focusout', function (e) { if (d.open && e.relatedTarget !== d.btn && !menu.contains(e.relatedTarget)) closeMenu(d, false); });
    placeDD(d);
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
    btn.addEventListener('click', function () { if (d.open) closeMenu(d, true); else { btn.focus(); openMenu(d); } }); // focus first: Safari does not focus buttons on click
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
  window.addEventListener('scroll', function (e) { var d = openDD; if (d && !d.menu.contains(e.target)) closeMenu(d, false); }, true);
  window.addEventListener('resize', function () { if (openDD) closeMenu(openDD, false); });

  // ---- search filter ----
  function wireSearch(root) {
    var s = $('[data-search]', root); if (!s || s._wired) return; s._wired = true;
    s.addEventListener('input', function () {
      var q = s.value.trim().toLowerCase();
      $$('[data-row]').forEach(function (r) { r.classList.toggle('hidden', q && r.getAttribute('data-row').toLowerCase().indexOf(q) < 0); });
    });
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
    if (val && el.maxLength >= 0 && val.length > el.maxLength) return 'Use at most ' + el.maxLength + ' characters.';
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
    p.textContent = ''; p.appendChild(icon('i-x'));
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
  var refreshing = false, pending = false, timer = null;
  function editing() {
    var a = document.activeElement;
    if (a && /^(INPUT|TEXTAREA|SELECT)$/.test(a.tagName) && a.form && !a.hasAttribute('data-search')) return true;
    if ($('#content[data-no-refresh]')) return true;
    if ($('#content [aria-invalid=true]')) return true;            // client-side field errors are showing              // a form re-rendered with a validation error
    if ($('.dd-menu')) return true;                                // a dropdown is open; replacing it would drop the menu
    var dlg = $('#confirm'); if (dlg && dlg.open) return true;     // a confirm is pending; replacing the form would detach it
    return $$('#content form').some(function (f) { return f._dirty; }); // unsaved edits
  }
  // Identity of a focusable element across a re-render: id, else href, else form action + button text, else name, else data-action + text.
  // An element with none of these is never restored, so focus cannot land on a different row's control.
  function focusKey(el) {
    if (el._sel) return 'sel:' + (el._sel.name || el._sel.id); // a dropdown button: its native select is the identity (the button id is regenerated)
    if (el.id) return 'id:' + el.id;
    var h = el.getAttribute('href'); if (h) return 'href:' + h;
    var f = el.form || (el.closest && el.closest('form'));
    if (f && f.getAttribute('action')) return 'form:' + f.getAttribute('action') + '|' + el.textContent.trim() + '|' + (el.name || '');
    if (el.name) return 'name:' + el.name;
    var da = el.getAttribute('data-action'); if (da) return 'act:' + da + '|' + el.textContent.trim();
    return '';
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
        var q = $('[data-search]'); var qv = q ? q.value : '';
        var FOC = 'button,a[href],input,select,textarea,summary', act = document.activeElement;
        var fk = act && main.contains(act) ? focusKey(act) : '', ftag = fk ? act.tagName : '';
        main.innerHTML = html;
        wire(main);
        if (fk && document.activeElement !== act) { // keep the keyboard position across a refresh, but only on the same element
          var again = $$(FOC, main).filter(function (x) { return x.tagName === ftag && focusKey(x) === fk; })[0];
          if (again) again.focus({ preventScroll: true });
        }
        if (qv) { var nq = $('[data-search]'); if (nq) { nq.value = qv; nq.dispatchEvent(new Event('input')); } }
        markUpdated();
      })
      .catch(function () { })
      .then(function () { refreshing = false; if (pending) { pending = false; schedule(); } });
  };
  function schedule() { clearTimeout(timer); timer = setTimeout(refresh, 250); }
  function connect() {
    if (!window.EventSource || !document.body.hasAttribute('data-live')) return;
    var es = new EventSource('/events');
    var dot = $('.live');
    es.onopen = function () { if (dot) dot.classList.remove('off'); schedule(); }; // catch up on anything missed while disconnected
    es.onmessage = function () { schedule(); };
    es.onerror = function () { if (dot) dot.classList.add('off'); };
  }

  function copyField(btn) {
    var inp = $(btn.getAttribute('data-copy-target')); if (!inp) return;
    var done = function () { toast('Link copied', 'ok'); };
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
    if (t && /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName)) { if (e.key === 'Escape') { e.target.blur(); closePanel(); } return; }
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.key === 'Escape') closePanel();
    if (document.body.classList.contains('panel-open')) return; // modal: no page shortcuts behind it
    if (e.key === 'n' && $('#panel')) { e.preventDefault(); openPanel(); }
    if (e.key === '/') { var s = $('[data-search]'); if (s) { e.preventDefault(); s.focus(); } }
    if (e.key === 't') cycleTheme();
  });

  function wireDirty(root) {
    $$('form', root).forEach(function (f) {
      if (f._dw) return; f._dw = true;
      f.addEventListener('input', function (e) { if (!e.target.hasAttribute('data-search')) f._dirty = true; });
    });
  }
  function wireTZ(root) {
    $$('input[name=tz_offset]', root).forEach(function (i) { i.value = String(new Date().getTimezoneOffset()); });
  }
  function wire(root) { wireValidation(root); wireDropdowns(root); wireConfirms(root); wireAsync(root); wireSearch(root); wireDirty(root); wireTZ(root); }
  document.addEventListener('DOMContentLoaded', function () {
    toastRegions(); liftFlash(); wire(document); wirePanel(); connect();
    markUpdated();
    setInterval(tickUpdated, 5000);
  });
})();
