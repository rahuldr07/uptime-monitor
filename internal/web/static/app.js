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
  function toastHost() {
    var h = $('.toasts');
    if (!h) { h = document.createElement('div'); h.className = 'toasts'; document.body.appendChild(h); }
    return h;
  }
  window.toast = function (msg, kind) {
    var t = document.createElement('div');
    t.className = 'toast' + (kind ? ' ' + kind : '');
    t.textContent = msg;
    var x = document.createElement('button'); x.textContent = '×'; x.setAttribute('aria-label', 'Dismiss');
    x.onclick = function () { t.remove(); };
    t.appendChild(x);
    toastHost().appendChild(t);
    setTimeout(function () { t.style.opacity = '0'; t.style.transition = 'opacity .3s'; setTimeout(function () { t.remove(); }, 300); }, 4500);
  };
  function liftFlash() {
    $$('[data-flash]').forEach(function (el) {
      toast(el.textContent.trim(), el.getAttribute('data-flash') === 'error' ? 'error' : '');
      el.remove();
    });
    if (location.search.indexOf('msg=') >= 0) {
      var u = new URL(location.href); u.searchParams.delete('msg');
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
        var btn = f.querySelector('button[type=submit]'); if (btn) btn.disabled = true;
        fetch(f.action, { method: 'POST', body: new FormData(f), headers: { 'Accept': 'application/json' }, credentials: 'same-origin' })
          .then(function (r) { if (r.status === 401) { sessionExpired(); throw 0; } return r.json().then(function (j) { j._status = r.status; return j; }); })
          .then(function (j) {
            if (j.error) { toast(j.error, 'error'); return; }
            if (j.message) toast(j.message, j.ok === false ? 'error' : 'ok');
            if (j.redirect) { location.href = j.redirect; } else { refresh(); }
          })
          .catch(function () { toast('Request failed', 'error'); })
          .then(function () { if (btn) btn.disabled = false; });
      });
    });
  }

  // ---- slide-over panel ----
  function sessionExpired() { location.href = '/login?next=' + encodeURIComponent(location.pathname + location.search); }
  window.openPanel = function () {
    document.body.classList.add('panel-open');
    var p = $('#panel'); if (!p) return;
    var r = $('[data-test-result]', p); if (r) { r.className = 'result'; r.textContent = ''; }
    setTimeout(function () { var u = $('#panel input[name=url]'); if (u) u.focus(); }, 60);
  };
  window.closePanel = function () { document.body.classList.remove('panel-open'); };
  function wirePanel() {
    var f = $('#panel form'); if (!f || f._wired) return; f._wired = true;
    var url = f.querySelector('input[name=url]'), name = f.querySelector('input[name=name]');
    var res = function () { return f.querySelector('[data-test-result]'); };
    url.addEventListener('blur', function () {
      if (name.value.trim() || !url.value.trim()) return;
      try { var h = new URL(url.value.trim()).hostname.replace(/^www\./, ''); name.value = h.charAt(0).toUpperCase() + h.slice(1); } catch (e) { }
    });
    var tb = f.querySelector('#test-btn'); if (tb) tb.addEventListener('click', function () {
      var r = res(); r.className = 'result'; r.textContent = 'Testing…'; r.style.display = 'block';
      fetch('/targets/test', { method: 'POST', body: new FormData(f), headers: { 'Accept': 'application/json' }, credentials: 'same-origin' })
        .then(function (x) { if (x.status === 401) { sessionExpired(); throw 0; } return x.json(); })
        .then(function (j) {
          r.style.display = '';
          if (j.error && !j.status_code) { r.className = 'result bad'; r.textContent = j.error; return; }
          r.className = 'result ' + (j.ok ? 'ok' : 'bad');
          r.textContent = (j.ok ? 'Test passed · ' : 'Test failed · ') + (j.status_code ? 'HTTP ' + j.status_code + ' ' : '') + 'in ' + j.latency_ms + ' ms' + (j.error ? ' · ' + j.error : '');
        })
        .catch(function () { r.className = 'result bad'; r.textContent = 'Could not run the test'; });
    });
    f.addEventListener('submit', function (e) {
      e.preventDefault();
      fetch(f.action, { method: 'POST', body: new FormData(f), headers: { 'Accept': 'application/json' }, credentials: 'same-origin' })
        .then(function (x) { if (x.status === 401) { sessionExpired(); throw 0; } return x.json(); })
        .then(function (j) {
          if (j.error) { var r = res(); r.className = 'result bad'; r.textContent = j.error; r.style.display = ''; return; }
          closePanel(); f.reset();
          if (j.redirect) location.href = j.redirect; else refresh();
        })
        .catch(function () { toast('Could not create the monitor', 'error'); });
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

  // ---- live refresh over SSE ----
  var refreshing = false, pending = false, timer = null;
  function editing() {
    var a = document.activeElement;
    if (a && /^(INPUT|TEXTAREA|SELECT)$/.test(a.tagName) && a.form && !a.hasAttribute('data-search')) return true;
    if ($('#content[data-no-refresh]')) return true;              // a form re-rendered with a validation error
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
        var u = $('[data-updated]'); if (u) u.textContent = 'Updated just now';
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
  function wire(root) { wireConfirms(root); wireAsync(root); wireSearch(root); wireDirty(root); wireTZ(root); }
  document.addEventListener('DOMContentLoaded', function () {
    liftFlash(); wire(document); wirePanel(); connect();
    setInterval(function () { var u = $('[data-updated]'); if (u && u._t) u.textContent = 'Updated ' + Math.round((Date.now() - u._t) / 1000) + 's ago'; }, 5000);
  });
})();
