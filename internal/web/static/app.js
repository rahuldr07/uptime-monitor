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
        $('#confirm p').textContent = f.getAttribute('data-confirm');
        d.returnValue = '';
        d.showModal();
        d.onclose = function () { if (d.returnValue === 'yes') { f._ok = true; f.submit(); } };
      });
    });
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
  window.openPanel = function () {
    document.body.classList.add('panel-open');
    var p = $('#panel'); if (!p) return;
    var r = $('[data-test-result]', p); if (r) clearResult(r);
    setTimeout(function () { var u = $('#panel input[name=url]'); if (u) u.focus(); }, 60);
  };
  window.closePanel = function () { document.body.classList.remove('panel-open'); };
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
    bad[0].focus();
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
    var dlg = $('#confirm'); if (dlg && dlg.open) return true;     // a confirm is pending; replacing the form would detach it
    return $$('#content form').some(function (f) { return f._dirty; }); // unsaved edits
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
        main.innerHTML = html;
        wire(main);
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
    if (e.target && /^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName)) { if (e.key === 'Escape') { e.target.blur(); closePanel(); } return; }
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.key === 'Escape') closePanel();
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
  function wire(root) { wireValidation(root); wireConfirms(root); wireAsync(root); wireSearch(root); wireDirty(root); wireTZ(root); }
  document.addEventListener('DOMContentLoaded', function () {
    toastRegions(); liftFlash(); wire(document); wirePanel(); connect();
    markUpdated();
    setInterval(tickUpdated, 5000);
  });
})();
