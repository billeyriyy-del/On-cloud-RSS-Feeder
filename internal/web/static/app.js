/* Noema web client — no build step, no dependencies, ES2017 (Safari 13+).
 *
 * Interaction model (mirrors the native apps):
 *   swipe right  → toggle read        swipe left → toggle saved
 *   long-press / right-click → preview sheet with actions
 *   pull down at the top of the list → refresh
 *   keyboard: j/k next/prev · o/Enter open · m read · s save · v original
 *             l listen · f full text · r refresh · Shift+A mark all read
 *             / search · . ask · z undo · g then u/s/a switch view · ? help
 * Every gesture has a visible button or key equivalent (VoiceOver and
 * Switch Control cannot swipe custom rows).
 */
(function () {
  'use strict';

  // ── Utilities ──────────────────────────────────────────────────────────
  var $ = function (sel, root) { return (root || document).querySelector(sel); };
  var now = function () { return Math.floor(Date.now() / 1000); };
  var store = {
    get: function (k, d) { try { var v = localStorage.getItem('noema.' + k); return v === null ? d : JSON.parse(v); } catch (e) { return d; } },
    set: function (k, v) { try { localStorage.setItem('noema.' + k, JSON.stringify(v)); } catch (e) { /* private mode */ } }
  };
  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }
  function icon(id) { return '<svg aria-hidden="true"><use href="#' + id + '"/></svg>'; }
  function ago(ts) {
    var d = now() - ts;
    if (d < 60) return 'now';
    if (d < 3600) return Math.floor(d / 60) + 'm';
    if (d < 86400) return Math.floor(d / 3600) + 'h';
    if (d < 7 * 86400) return Math.floor(d / 86400) + 'd';
    return new Date(ts * 1000).toLocaleDateString(undefined, { day: 'numeric', month: 'short' });
  }
  function fullDate(ts) {
    return new Date(ts * 1000).toLocaleString(undefined, { year: 'numeric', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
  }
  function duration(secs) {
    if (!secs) return '';
    var m = Math.max(1, Math.round(secs / 60));
    return m < 60 ? m + ' min' : Math.floor(m / 60) + ' h ' + (m % 60) + ' min';
  }
  function announce(text) {
    var el = $('#announce');
    el.textContent = '';
    setTimeout(function () { el.textContent = text; }, 30);
  }
  var reducedMotion = window.matchMedia && matchMedia('(prefers-reduced-motion: reduce)').matches;
  function narrow() { return window.matchMedia('(max-width: 699.98px)').matches; }
  function drawerMode() { return window.matchMedia('(max-width: 1099.98px)').matches; }

  // ── API ────────────────────────────────────────────────────────────────
  function api(method, path, body) {
    var opts = { method: method, credentials: 'same-origin', headers: { 'X-Noema-Client': 'web', 'Accept': 'application/json' } };
    if (body !== undefined) {
      if (typeof body === 'string') { opts.headers['Content-Type'] = 'text/xml'; opts.body = body; }
      else { opts.headers['Content-Type'] = 'application/json'; opts.body = JSON.stringify(body); }
    }
    return fetch(path, opts).then(function (r) {
      if (r.status === 401 && path !== '/auth/login') { showLogin(); throw Object.assign(new Error('Signed out'), { status: 401 }); }
      var ct = r.headers.get('Content-Type') || '';
      return (ct.indexOf('json') >= 0 ? r.json() : r.text()).then(function (data) {
        if (!r.ok) throw Object.assign(new Error((data && data.error) || ('HTTP ' + r.status)), { status: r.status, data: data });
        return data;
      });
    });
  }

  // ── State ──────────────────────────────────────────────────────────────
  var S = {
    view: 'unread', sourceId: 0, folderId: 0, q: '',
    items: [], byId: {}, listed: {}, next: '', loading: false, done: false, maxId: 0,
    sel: -1, openId: 0,
    sources: [], sourceById: {}, folders: [], counts: { unread: 0, starred: 0, by_source: {}, max_id: 0 },
    expanded: store.get('expanded', {}),
    voiceCtx: null, gPrefix: false
  };
  var queue = store.get('queue', []);

  // ── Boot & auth ────────────────────────────────────────────────────────
  function drawMark() {
    var d = '', n = 360;
    for (var i = 0; i <= n; i++) {
      var t = 2 * Math.PI * i / n;
      d += (i ? 'L' : 'M') + Math.sin(3 * t + Math.PI / 2).toFixed(4) + ' ' + Math.sin(4 * t).toFixed(4);
    }
    $('#mark-path').setAttribute('d', d + 'Z');
  }

  function showLogin() {
    $('#app').hidden = true; $('#voice-fab').hidden = true; $('#voice').hidden = true;
    $('#login').hidden = false;
    setTimeout(function () { $('#password').focus(); }, 50);
  }

  $('#login-form').addEventListener('submit', function (e) {
    e.preventDefault();
    var err = $('#login-error');
    err.textContent = '';
    api('POST', '/auth/login', { password: $('#password').value, cookie: true }).then(function () {
      $('#password').value = '';
      $('#login').hidden = true;
      start();
    }).catch(function (ex) { err.textContent = ex.message; $('#password').select(); });
  });

  var started = false;
  function start() {
    $('#app').hidden = false;
    $('#voice-fab').hidden = false;
    if (!started) { started = true; bind(); }
    route(true);
    Promise.all([loadSources(), loadCounts()]).then(renderTree);
    flush();
  }

  function boot() {
    drawMark();
    if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(function () {});
    var size = store.get('readerSize', 0);
    if (size) document.documentElement.style.setProperty('--reader-size', size + 'rem');
    api('GET', '/auth/check').then(start).catch(function (e) { if (e.status !== 401) showLogin(); });
  }

  // ── Routing ────────────────────────────────────────────────────────────
  function route(initial) {
    var m = location.pathname.match(/^\/item\/(\d+)/);
    var p = new URLSearchParams(location.search);
    if (!m) {
      var src = +p.get('source') || 0, fol = +p.get('folder') || 0;
      var view = p.get('view') || (src || fol ? 'all' : 'unread');
      // Coming back from a story keeps the list (and its scroll position).
      if (initial || src !== S.sourceId || fol !== S.folderId || view !== S.view) {
        S.sourceId = src; S.folderId = fol; S.view = view; S.q = '';
        loadList(true);
      }
    } else if (initial) {
      loadList(true);
    }
    if (m) openItem(+m[1], false); else closeReader(false);
  }
  window.addEventListener('popstate', function () { route(false); });

  function listURL() {
    var q = S.sourceId ? 'source=' + S.sourceId : S.folderId ? 'folder=' + S.folderId : '';
    var defaultView = (S.sourceId || S.folderId) ? 'all' : 'unread';
    if (S.view !== defaultView) q += (q ? '&' : '') + 'view=' + S.view;
    return q ? '/?' + q : '/';
  }
  function select(opts) {
    S.view = opts.view || 'all'; S.sourceId = opts.sourceId || 0; S.folderId = opts.folderId || 0; S.q = '';
    $('#search').value = '';
    history.replaceState(null, '', listURL());
    closeDrawer();
    loadList(true);
    renderTree();
  }

  // ── Sidebar ────────────────────────────────────────────────────────────
  function loadSources() {
    return Promise.all([api('GET', '/sources'), api('GET', '/folders')]).then(function (r) {
      S.sources = r[0]; S.folders = r[1]; S.sourceById = {};
      S.sources.forEach(function (s) { S.sourceById[s.id] = s; });
    });
  }
  function loadCounts() {
    return api('GET', '/counts').then(function (c) {
      var fresh = S.counts.max_id && c.max_id > S.counts.max_id;
      S.counts = c;
      renderCounts();
      return fresh;
    });
  }
  function folderUnread(fid) {
    var n = 0;
    S.sources.forEach(function (s) { if (s.folder_id === fid) n += S.counts.by_source[s.id] || 0; });
    S.folders.forEach(function (f) { if (f.parent_id === fid) n += folderUnread(f.id); });
    return n;
  }
  function renderCounts() {
    $('#count-unread').textContent = S.counts.unread || '';
    $('#count-starred').textContent = S.counts.starred || '';
    var title = S.counts.unread ? '(' + S.counts.unread + ') Noema' : 'Noema';
    if (document.title !== title) document.title = title;
    if (navigator.setAppBadge) { try { S.counts.unread ? navigator.setAppBadge(S.counts.unread) : navigator.clearAppBadge(); } catch (e) {} }
    Array.prototype.forEach.call(document.querySelectorAll('[data-count-source]'), function (el) {
      el.textContent = S.counts.by_source[el.getAttribute('data-count-source')] || '';
    });
    Array.prototype.forEach.call(document.querySelectorAll('[data-count-folder]'), function (el) {
      el.textContent = folderUnread(+el.getAttribute('data-count-folder')) || '';
    });
  }
  function renderTree() {
    Array.prototype.forEach.call(document.querySelectorAll('.view-link'), function (a) {
      var on = !S.sourceId && !S.folderId && !S.q && a.getAttribute('data-view') === S.view;
      if (on) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
    });
    function sourceRow(s) {
      var cls = 'tree-row' + (s.is_dead ? ' dead' : '') + (s.last_error && !s.is_dead ? ' err' : '');
      var cur = S.sourceId === s.id ? ' aria-current="page"' : '';
      var kind = s.kind === 'audio' ? 'i-audio' : s.kind === 'video' ? 'i-video' : 'i-dot';
      var status = s.is_dead ? ', stopped updating' : s.last_error ? ', last update failed' : '';
      return '<li><a class="' + cls + '" href="/?source=' + s.id + '" data-source="' + s.id + '"' + cur +
        ' aria-label="' + esc(s.title + status) + '">' + icon(kind) + '<span class="name">' + esc(s.title) +
        '</span><span class="count" data-count-source="' + s.id + '"></span></a></li>';
    }
    function folderNode(f) {
      var open = S.expanded[f.id] !== false;
      var kids = S.folders.filter(function (c) { return c.parent_id === f.id; }).map(folderNode).join('') +
        S.sources.filter(function (s) { return s.folder_id === f.id; }).map(sourceRow).join('');
      var cur = S.folderId === f.id ? ' aria-current="page"' : '';
      return '<li class="tree-folder" aria-expanded="' + open + '"><a class="tree-row" href="/?folder=' + f.id + '" data-folder="' + f.id + '"' + cur + '>' +
        '<button class="tree-toggle" type="button" data-toggle="' + f.id + '" aria-label="' + (open ? 'Collapse ' : 'Expand ') + esc(f.name) + '" aria-expanded="' + open + '">' + icon('i-chev') + '</button>' +
        '<span class="name">' + esc(f.name) + '</span><span class="count" data-count-folder="' + f.id + '"></span></a>' +
        '<ul role="list"' + (open ? '' : ' hidden') + '>' + kids + '</ul></li>';
    }
    var html = '<ul role="list">' + S.folders.filter(function (f) { return !f.parent_id; }).map(folderNode).join('') +
      S.sources.filter(function (s) { return !s.folder_id; }).map(sourceRow).join('') + '</ul>';
    if (!S.sources.length) html = '<p class="muted" style="padding:0 0.6rem">No feeds yet. Add one below.</p>';
    $('#tree').innerHTML = html;
    renderCounts();
  }

  // ── List ───────────────────────────────────────────────────────────────
  function listTitle() {
    if (S.q) return 'Search: ' + S.q;
    if (S.sourceId) return (S.sourceById[S.sourceId] || {}).title || 'Feed';
    if (S.folderId) { var f = S.folders.filter(function (x) { return x.id === S.folderId; })[0]; return f ? f.name : 'Folder'; }
    return { unread: 'Unread', starred: 'Saved', all: 'All stories' }[S.view] || 'Stories';
  }
  var listToken = 0;
  function loadList(reset) {
    if (reset) {
      S.items = []; S.listed = {}; S.next = ''; S.done = false; S.sel = -1; S.maxId = S.counts.max_id || 0;
      $('#list').innerHTML = '';
      $('#listscroll').scrollTop = 0;
      $('#list-title').textContent = listTitle();
      $('#mark-all').hidden = S.view === 'starred' && !S.sourceId && !S.folderId;
      listToken++;
    }
    if (S.loading || S.done) return Promise.resolve();
    S.loading = true;
    var token = listToken;
    var p = new URLSearchParams();
    p.set('filter', S.q ? 'all' : S.view);
    p.set('limit', '40');
    if (S.sourceId) p.set('source_id', S.sourceId);
    if (S.folderId) p.set('folder_id', S.folderId);
    if (S.q) p.set('q', S.q);
    if (S.next) p.set('cursor', S.next);
    $('#list-foot').textContent = 'Loading…';
    return api('GET', '/items?' + p.toString()).then(function (r) {
      if (token !== listToken) return;
      var frag = document.createDocumentFragment();
      r.items.forEach(function (fresh) {
        if (S.listed[fresh.id]) return; // the list dedupes separately from the item cache
        S.listed[fresh.id] = true;
        var it = S.byId[fresh.id] ? Object.assign(S.byId[fresh.id], fresh) : fresh;
        S.byId[it.id] = it;
        S.items.push(it);
        if (it.id > S.maxId) S.maxId = it.id;
        frag.appendChild(rowFor(it));
      });
      $('#list').appendChild(frag);
      S.next = r.next_cursor;
      S.done = !r.next_cursor;
      $('#list-foot').textContent = S.items.length ? (S.done ? '' : '') :
        (S.q ? 'Nothing matches “' + S.q + '”.' : S.view === 'unread' ? 'You’re all caught up.' : S.view === 'starred' ? 'Nothing saved yet. Swipe left on a story to save it.' : 'No stories yet.');
    }).catch(function (e) {
      if (token === listToken) $('#list-foot').textContent = navigator.onLine ? 'Couldn’t load stories: ' + e.message : 'You’re offline.';
    }).then(function () { S.loading = false; });
  }

  function rowLabel(it) {
    return it.title + ', ' + it.source_title + ', ' + ago(it.published_at) +
      (it.is_read ? '' : ', unread') + (it.is_starred ? ', saved' : '') +
      (it.kind === 'audio' ? ', podcast' : it.kind === 'video' ? ', video' : '');
  }
  function rowFor(it) {
    var li = document.createElement('li');
    li.className = 'row-wrap';
    li.setAttribute('data-id', it.id);
    li.innerHTML = rowHTML(it);
    bindRow(li, it);
    return li;
  }
  function rowHTML(it) {
    var kind = it.kind === 'audio' ? icon('i-audio') : it.kind === 'video' ? icon('i-video') : '';
    var thumb = it.image_url ? '<img class="r-thumb" src="' + esc(it.image_url) + '" alt="" loading="lazy" decoding="async" referrerpolicy="no-referrer">' : '';
    var time = it.reading_secs ? '<span class="sep">·</span><span>' + duration(it.reading_secs) + '</span>' : '';
    return '<div class="row-under" aria-hidden="true"><span class="u-left">' + icon('i-check') + (it.is_read ? 'Unread' : 'Read') + '</span>' +
      '<span class="u-right">' + (it.is_starred ? 'Unsave' : 'Save') + icon('i-star') + '</span></div>' +
      '<div class="row' + (it.is_read ? ' read' : '') + '"' + (S.openId === it.id ? ' aria-current="true"' : '') + '>' +
      '<span class="unread-dot" aria-hidden="true"></span>' +
      '<a class="r-link" href="/item/' + it.id + '" aria-label="' + esc(rowLabel(it)) + '" draggable="false">' +
      '<span class="r-text"><span class="r-meta"><span class="src">' + esc(it.source_title) + '</span><span class="sep">·</span><span>' + ago(it.published_at) + '</span>' + time + kind +
      (it.is_starred ? '<span class="star">' + icon('i-star-fill') + '</span>' : '') + '</span>' +
      '<span class="r-title">' + esc(it.title) + '</span>' +
      (it.summary ? '<span class="r-sum">' + esc(it.summary) + '</span>' : '') + '</span>' + thumb + '</a>' +
      '<span class="r-actions"><button class="icon-btn" type="button" data-act="read" aria-label="' + (it.is_read ? 'Mark unread' : 'Mark read') + '">' + icon(it.is_read ? 'i-dot' : 'i-check') + '</button>' +
      '<button class="icon-btn" type="button" data-act="star" aria-label="' + (it.is_starred ? 'Unsave' : 'Save') + '">' + icon(it.is_starred ? 'i-star-fill' : 'i-star') + '</button>' +
      '<button class="icon-btn" type="button" data-act="more" aria-label="More actions">' + icon('i-more') + '</button></span></div>';
  }
  function refreshRow(it) {
    var li = document.querySelector('.row-wrap[data-id="' + it.id + '"]');
    if (!li) return;
    var hadFocus = li.contains(document.activeElement);
    li.innerHTML = rowHTML(it);
    bindRow(li, it);
    if (hadFocus) $('.r-link', li).focus();
  }

  // Swipe, long-press and click handling for one row.
  function bindRow(li, it) {
    var row = $('.row', li), link = $('.r-link', li);
    var sx = 0, sy = 0, dx = 0, pid = null, tracking = false, decided = false, horizontal = false, moved = false, lp = 0, suppress = false;
    function reset() {
      row.classList.remove('dragging');
      row.style.transform = '';
      li.classList.remove('swipe-left', 'swipe-right');
    }
    row.addEventListener('pointerdown', function (e) {
      if (e.pointerType === 'mouse' && e.button !== 0) return;
      if (e.target.closest && e.target.closest('.r-actions')) return;
      sx = e.clientX; sy = e.clientY; dx = 0; pid = e.pointerId;
      tracking = true; decided = false; horizontal = false; moved = false;
      clearTimeout(lp);
      lp = setTimeout(function () {
        if (!moved && tracking) {
          tracking = false; suppress = true;
          if (navigator.vibrate) navigator.vibrate(8);
          openPreview(it);
        }
      }, 480);
    });
    row.addEventListener('pointermove', function (e) {
      if (!tracking || e.pointerId !== pid) return;
      var mx = e.clientX - sx, my = e.clientY - sy;
      if (Math.abs(mx) > 6 || Math.abs(my) > 6) { moved = true; clearTimeout(lp); }
      if (!decided && (Math.abs(mx) > 10 || Math.abs(my) > 10)) {
        decided = true;
        horizontal = e.pointerType !== 'mouse' && Math.abs(mx) > Math.abs(my) * 1.3;
        if (horizontal) { row.classList.add('dragging'); try { row.setPointerCapture(pid); } catch (x) {} }
        else tracking = false;
      }
      if (!horizontal) return;
      e.preventDefault();
      dx = mx > 0 ? Math.min(mx, 140 + (mx - 140) * 0.25) : Math.max(mx, -140 + (mx + 140) * 0.25);
      row.style.transform = 'translateX(' + dx + 'px)';
      li.classList.toggle('swipe-right', dx > 0);
      li.classList.toggle('swipe-left', dx < 0);
    });
    function end() {
      clearTimeout(lp);
      if (!tracking && !horizontal) return;
      tracking = false;
      if (horizontal) {
        suppress = true;
        var d = dx; horizontal = false;
        reset();
        if (d > 90) toggleRead(it, true);
        else if (d < -90) toggleStar(it, true);
      }
    }
    row.addEventListener('pointerup', end);
    row.addEventListener('pointercancel', function () { clearTimeout(lp); tracking = false; horizontal = false; reset(); });
    row.addEventListener('contextmenu', function (e) { e.preventDefault(); clearTimeout(lp); if (!suppress) openPreview(it); });
    link.addEventListener('click', function (e) {
      if (suppress) { e.preventDefault(); setTimeout(function () { suppress = false; }, 0); return; }
      if (e.metaKey || e.ctrlKey || e.shiftKey || e.button === 1) return; // let the browser open a tab
      e.preventDefault();
      openItem(it.id, true);
    });
    link.addEventListener('focus', function () { S.sel = S.items.indexOf(it); });
    li.addEventListener('click', function (e) {
      var b = e.target.closest && e.target.closest('[data-act]');
      if (!b) return;
      e.preventDefault();
      var act = b.getAttribute('data-act');
      if (act === 'read') toggleRead(it);
      else if (act === 'star') toggleStar(it);
      else openPreview(it);
    });
  }

  // Pull to refresh (touch only: mice have the refresh button and "r").
  function bindPullToRefresh() {
    var scroller = $('#listscroll'), ptr = $('#ptr'), y0 = -1, pulled = 0;
    scroller.addEventListener('touchstart', function (e) { y0 = scroller.scrollTop <= 0 ? e.touches[0].clientY : -1; pulled = 0; }, { passive: true });
    scroller.addEventListener('touchmove', function (e) {
      if (y0 < 0) return;
      pulled = e.touches[0].clientY - y0;
      if (pulled <= 0) { ptr.style.opacity = 0; return; }
      if (e.cancelable && pulled > 8) e.preventDefault();
      var p = Math.min(pulled, 120);
      ptr.style.opacity = Math.min(1, p / 70);
      ptr.style.transform = 'translateY(' + (p * 0.6 - 16) + 'px) rotate(' + p * 3 + 'deg)';
    }, { passive: false });
    scroller.addEventListener('touchend', function () {
      if (y0 < 0) return;
      y0 = -1;
      if (pulled > 70) { ptr.classList.add('spin'); refresh().then(function () { ptr.classList.remove('spin'); ptr.style.opacity = 0; }); }
      else { ptr.style.opacity = 0; ptr.style.transform = ''; }
    });
  }

  function refresh() {
    var btn = $('#refresh');
    btn.setAttribute('aria-busy', 'true');
    announce('Refreshing');
    return api('POST', '/sources/refresh', {}).catch(function () {}).then(function () {
      return new Promise(function (res) { setTimeout(res, 3500); });
    }).then(function () {
      return Promise.all([loadCounts(), loadSources()]);
    }).then(function () {
      renderTree();
      if ($('#listscroll').scrollTop < 200) loadList(true);
      btn.removeAttribute('aria-busy');
      announce('Up to date');
    });
  }

  // ── State changes (optimistic, queued, last-write-wins on the server) ───
  function setState(it, patch, quiet) {
    var op = { id: it.id, at: now() };
    var before = { is_read: it.is_read, is_starred: it.is_starred };
    if ('is_read' in patch && patch.is_read !== it.is_read) {
      op.is_read = patch.is_read;
      S.counts.unread += patch.is_read ? -1 : 1;
      S.counts.by_source[it.source_id] = Math.max(0, (S.counts.by_source[it.source_id] || 0) + (patch.is_read ? -1 : 1));
      it.is_read = patch.is_read;
    }
    if ('is_starred' in patch && patch.is_starred !== it.is_starred) {
      op.is_starred = patch.is_starred;
      S.counts.starred += patch.is_starred ? 1 : -1;
      it.is_starred = patch.is_starred;
    }
    if (!('is_read' in op) && !('is_starred' in op)) return;
    queue.push(op);
    store.set('queue', queue);
    refreshRow(it);
    renderCounts();
    if (S.openId === it.id) renderStoryActions(it);
    flush();
    return before;
  }
  function toggleRead(it, fromGesture) {
    var before = setState(it, { is_read: !it.is_read });
    var msg = it.is_read ? 'Marked as read' : 'Marked as unread';
    if (fromGesture) toast(msg, function () { setState(it, { is_read: before.is_read }); });
    else announce(msg);
  }
  function toggleStar(it, fromGesture) {
    var before = setState(it, { is_starred: !it.is_starred });
    var msg = it.is_starred ? 'Saved' : 'Removed from saved';
    if (fromGesture) toast(msg, function () { setState(it, { is_starred: before.is_starred }); });
    else announce(msg);
  }
  var flushing = false;
  function flush() {
    if (flushing || !queue.length) return;
    flushing = true;
    var batch = queue.slice(0, 500);
    api('POST', '/items/state', { ops: batch }).then(function () {
      queue.splice(0, batch.length);
    }).catch(function (e) {
      if (e.status && e.status >= 400 && e.status < 500 && e.status !== 401) queue.splice(0, batch.length); // unrecoverable
    }).then(function () {
      store.set('queue', queue);
      flushing = false;
      if (queue.length && navigator.onLine) setTimeout(flush, 5000);
    });
  }
  window.addEventListener('online', flush);

  function markAll() {
    var body = { source_id: S.sourceId, folder_id: S.folderId, max_id: S.maxId, at: now() };
    if (S.q || S.view === 'starred') body = { ids: S.items.filter(function (i) { return !i.is_read; }).map(function (i) { return i.id; }), at: now() };
    if (body.ids && !body.ids.length) return;
    api('POST', '/items/mark-read', body).then(function (r) {
      var ids = r.ids;
      applyRead(ids, true);
      toast('Marked ' + ids.length + ' as read', function () {
        api('POST', '/items/mark-read', { ids: ids, read: false, at: now() }).then(function () { applyRead(ids, false); loadCounts(); });
      });
      loadCounts();
    }).catch(function (e) { toast('Couldn’t mark as read: ' + e.message); });
  }
  function applyRead(ids, read) {
    ids.forEach(function (id) {
      var it = S.byId[id];
      if (it && it.is_read !== read) { it.is_read = read; refreshRow(it); }
    });
  }

  // ── Reader ─────────────────────────────────────────────────────────────
  var openToken = 0;
  function openItem(id, push) {
    var token = ++openToken;
    var cached = S.byId[id];
    S.openId = id;
    Array.prototype.forEach.call(document.querySelectorAll('.row[aria-current]'), function (r) { r.removeAttribute('aria-current'); });
    var li = document.querySelector('.row-wrap[data-id="' + id + '"]');
    if (li) { $('.row', li).setAttribute('aria-current', 'true'); S.sel = S.items.indexOf(cached); }
    if (push && narrow()) history.pushState({ item: id }, '', '/item/' + id);
    else if (location.pathname !== '/item/' + id) history.replaceState(null, '', '/item/' + id);
    $('#app').classList.add('reading');
    $('#reader-empty').hidden = true;
    $('#story').hidden = false;
    if (cached) renderStory(cached, true);
    return api('GET', '/items/' + id).then(function (r) {
      if (token !== openToken) return;
      var it = Object.assign(cached || {}, r.item);
      S.byId[id] = it;
      renderStory(it, false);
      if (!it.is_read) setState(it, { is_read: true });
      if (narrow() || push) $('#reader').focus();
    }).catch(function (e) {
      if (token === openToken) $('#story-body').innerHTML = '<p class="error-text">Couldn’t open this story: ' + esc(e.message) + '</p>';
    });
  }
  function closeReader(pop) {
    S.openId = 0;
    $('#app').classList.remove('reading');
    if (pop && history.state && history.state.item) { history.back(); return; }
    if (location.pathname.indexOf('/item/') === 0) history.replaceState(null, '', listURL());
    if (!narrow()) { $('#story').hidden = true; $('#reader-empty').hidden = false; }
    var li = S.sel >= 0 && S.items[S.sel] && document.querySelector('.row-wrap[data-id="' + S.items[S.sel].id + '"] .r-link');
    if (li && narrow()) li.focus();
  }
  function youtubeID(url) {
    var m = /(?:youtube\.com\/watch\?v=|youtu\.be\/|youtube\.com\/shorts\/)([\w-]{6,})/.exec(url || '');
    return m ? m[1] : '';
  }
  function renderStory(it, partial) {
    var body = $('#story-body');
    var media = '';
    var enc = (it.enclosures || []).filter(function (e) { return /^(audio|video)\//.test(e.type || ''); })[0];
    if (enc && /^audio\//.test(enc.type)) {
      media = '<audio class="story-audio" controls preload="none" src="' + esc(enc.url) + '"></audio>';
    } else if (enc) {
      media = '<video class="story-audio" controls preload="none" playsinline src="' + esc(enc.url) + '"' + (it.image_url ? ' poster="' + esc(it.image_url) + '"' : '') + '></video>';
    } else if (it.kind === 'video' && it.image_url) {
      media = '<a class="media-card" href="' + esc(it.url) + '" target="_blank" rel="noopener noreferrer" aria-label="Watch ' + esc(it.title) + '"><img src="' + esc(it.image_url) + '" alt="" referrerpolicy="no-referrer"><span class="play">' + icon('i-play') + '</span></a>';
    } else if (it.image_url && (it.content_html || '').indexOf(it.image_url) < 0) {
      media = '<img class="story-hero" src="' + esc(it.image_url) + '" alt="" referrerpolicy="no-referrer">';
    }
    var sub = [it.author ? 'By ' + esc(it.author) : '', it.reading_secs ? esc(duration(it.reading_secs)) + (it.kind === 'article' ? ' read' : '') : ''].filter(Boolean).join(' · ');
    var content = partial ? '<p class="muted">' + esc(it.summary || '') + '</p>' : (it.content_html || '<p class="muted">This story has no text. Open the original to read it.</p>');
    body.innerHTML =
      '<p class="story-meta"><a href="/?source=' + it.source_id + '" data-source="' + it.source_id + '">' + esc(it.source_title) + '</a> · <time datetime="' + new Date(it.published_at * 1000).toISOString() + '">' + fullDate(it.published_at) + '</time></p>' +
      '<h1 class="story-title" id="story-title">' + esc(it.title) + '</h1>' +
      (sub ? '<p class="story-sub">' + sub + '</p>' : '') + media +
      (it._fullError ? '<p class="story-note">Couldn’t load the full article (' + esc(it._fullError) + '). Showing the feed’s version.</p>' : '') +
      '<div class="content"' + (it.lang ? ' lang="' + esc(it.lang) + '"' : '') + '>' + content + '</div>';
    $('#story').setAttribute('aria-labelledby', 'story-title');
    $('#act-open').href = it.url || '#';
    renderStoryActions(it);
    if (!partial) $('#reader').scrollTop = 0;
  }
  function renderStoryActions(it) {
    var r = $('#act-read'), s = $('#act-star'), f = $('#act-full');
    r.setAttribute('aria-pressed', String(!!it.is_read));
    r.setAttribute('aria-label', (it.is_read ? 'Mark unread' : 'Mark read') + ' (m)');
    $('use', r).setAttribute('href', it.is_read ? '#i-check' : '#i-dot');
    s.setAttribute('aria-pressed', String(!!it.is_starred));
    s.setAttribute('aria-label', (it.is_starred ? 'Unsave' : 'Save') + ' (s)');
    $('use', s).setAttribute('href', it.is_starred ? '#i-star-fill' : '#i-star');
    f.hidden = !!it.has_full_text || it.kind !== 'article' || !it.url;
  }
  function loadFullText() {
    var it = S.byId[S.openId];
    if (!it) return;
    announce('Loading full article');
    $('#act-full').setAttribute('aria-busy', 'true');
    api('GET', '/items/' + it.id + '?full=1&retry=1').then(function (r) {
      Object.assign(it, r.item);
      it._fullError = r.full_text_error || '';
      renderStory(it, false);
      announce(r.full_text_error ? 'Full article unavailable' : 'Full article loaded');
    }).catch(function (e) { toast(e.message); }).then(function () { $('#act-full').removeAttribute('aria-busy'); });
  }
  function cycleSize() {
    var sizes = [1, 1.125, 1.25, 1.375, 1.5, 1.75];
    var cur = store.get('readerSize', 1.125);
    var next = sizes[(sizes.indexOf(cur) + 1) % sizes.length];
    store.set('readerSize', next);
    document.documentElement.style.setProperty('--reader-size', next + 'rem');
    announce('Text size ' + Math.round(next * 100) + '%');
  }

  // ── Navigation helpers ─────────────────────────────────────────────────
  function move(step, open) {
    if (!S.items.length) return;
    var i = S.sel < 0 ? (step > 0 ? 0 : S.items.length - 1) : Math.max(0, Math.min(S.items.length - 1, S.sel + step));
    S.sel = i;
    var it = S.items[i];
    var link = document.querySelector('.row-wrap[data-id="' + it.id + '"] .r-link');
    if (link) {
      link.focus({ preventScroll: true });
      if (link.scrollIntoView) link.scrollIntoView({ block: 'nearest', behavior: reducedMotion ? 'auto' : 'smooth' });
    }
    if (open && !narrow()) openItem(it.id, false);
    if (i >= S.items.length - 5) loadList(false);
  }
  function current() { return S.byId[S.openId] || S.items[S.sel]; }

  // ── Sheets (preview, add feed, settings, help) ─────────────────────────
  var sheetReturn = null;
  function openSheet(html, label) {
    sheetReturn = document.activeElement;
    var sheet = $('#sheet');
    sheet.innerHTML = '<div class="grab" aria-hidden="true"></div>' + html;
    sheet.setAttribute('aria-labelledby', 'sheet-title');
    if (label) sheet.setAttribute('aria-label', label);
    $('#sheet-wrap').hidden = false;
    var first = sheet.querySelector('input, button, select, a[href]');
    (first || sheet).focus();
  }
  function closeSheet() {
    $('#sheet-wrap').hidden = true;
    $('#sheet').innerHTML = '';
    if (sheetReturn && sheetReturn.focus) sheetReturn.focus();
  }
  function sheetOpen() { return !$('#sheet-wrap').hidden; }

  function openPreview(it) {
    openSheet(
      (it.image_url ? '<img class="prev-img" src="' + esc(it.image_url) + '" alt="" referrerpolicy="no-referrer">' : '') +
      '<p class="story-meta">' + esc(it.source_title) + ' · ' + ago(it.published_at) + (it.reading_secs ? ' · ' + duration(it.reading_secs) : '') + '</p>' +
      '<h2 id="sheet-title">' + esc(it.title) + '</h2>' +
      '<p class="prev-sum">' + esc(it.summary || '') + '</p>' +
      '<div class="sheet-actions">' +
      '<button class="btn-primary" type="button" data-p="open">Open</button>' +
      '<button class="btn-quiet" type="button" data-p="read">' + icon(it.is_read ? 'i-dot' : 'i-check') + (it.is_read ? 'Mark unread' : 'Mark read') + '</button>' +
      '<button class="btn-quiet" type="button" data-p="star">' + icon(it.is_starred ? 'i-star-fill' : 'i-star') + (it.is_starred ? 'Unsave' : 'Save') + '</button>' +
      '<button class="btn-quiet" type="button" data-p="listen">' + icon('i-listen') + 'Listen</button>' +
      (it.url ? '<a class="btn-quiet" href="' + esc(it.url) + '" target="_blank" rel="noopener noreferrer">' + icon('i-external') + 'Original</a>' : '') +
      '<button class="btn-quiet" type="button" data-p="older">' + icon('i-check') + 'Read all older</button>' +
      '</div>');
    $('#sheet').onclick = function (e) {
      var b = e.target.closest && e.target.closest('[data-p]');
      if (!b) return;
      var a = b.getAttribute('data-p');
      closeSheet();
      if (a === 'open') openItem(it.id, true);
      else if (a === 'read') toggleRead(it);
      else if (a === 'star') toggleStar(it);
      else if (a === 'listen') listen(it);
      else if (a === 'older') {
        api('POST', '/items/mark-read', { source_id: S.sourceId, folder_id: S.folderId, before: it.published_at + 1, max_id: S.maxId, at: now() }).then(function (r) {
          applyRead(r.ids, true); loadCounts();
          toast('Marked ' + r.ids.length + ' older as read', function () {
            api('POST', '/items/mark-read', { ids: r.ids, read: false, at: now() }).then(function () { applyRead(r.ids, false); loadCounts(); });
          });
        });
      }
    };
  }

  function folderOptions(selected) {
    var out = '<option value="">No folder</option>';
    function walk(parent, depth) {
      S.folders.filter(function (f) { return (f.parent_id || 0) === parent; }).forEach(function (f) {
        out += '<option value="' + f.id + '"' + (f.id === selected ? ' selected' : '') + '>' + new Array(depth + 1).join(' ') + esc(f.name) + '</option>';
        walk(f.id, depth + 1);
      });
    }
    walk(0, 0);
    return out;
  }

  function openAddFeed() {
    openSheet(
      '<h2 id="sheet-title">Add a feed</h2>' +
      '<p class="muted">Paste a site, feed, YouTube channel, subreddit, GitHub repo, podcast or Mastodon link.</p>' +
      '<form id="add-form"><label class="field"><span>Address</span><input id="add-url" type="url" inputmode="url" autocomplete="url" placeholder="https://" required></label>' +
      '<label class="field"><span>Folder</span><select id="add-folder">' + folderOptions(S.folderId) + '</select></label>' +
      '<label class="check"><input id="add-full" type="checkbox"><span>Fetch full articles (for feeds that only send a teaser)</span></label>' +
      '<button class="btn-primary" type="submit">Find feeds</button></form><div id="add-results" aria-live="polite"></div>');
    $('#add-form').onsubmit = function (e) {
      e.preventDefault();
      var out = $('#add-results');
      out.innerHTML = '<p class="muted">Looking…</p>';
      api('POST', '/discover', { url: $('#add-url').value }).then(function (r) {
        out.innerHTML = '<ul class="cands" role="list">' + r.candidates.map(function (c, i) {
          var what = c.type === 'html' ? 'No feed found — follow this page by scraping it' : (c.kind === 'audio' ? 'Podcast' : c.kind === 'video' ? 'Video feed' : 'Feed') + (c.items ? ' · ' + c.items + ' recent items' : '');
          return '<li><button type="button" data-i="' + i + '"><strong>' + esc(c.title || c.url) + '</strong><small>' + esc(what) + '</small><small>' + esc(c.url) + '</small></button></li>';
        }).join('') + '</ul>';
        out.onclick = function (ev) {
          var b = ev.target.closest && ev.target.closest('[data-i]');
          if (!b) return;
          var c = r.candidates[+b.getAttribute('data-i')];
          var fid = +$('#add-folder').value || null;
          api('POST', '/sources', { url: c.url, type: c.type, title: c.title, folder_id: fid, fetch_full_text: $('#add-full').checked }).then(function (res) {
            closeSheet();
            toast('Subscribed to ' + res.source.title);
            return loadSources().then(function () { select({ view: 'all', sourceId: res.source.id }); setTimeout(function () { loadList(true); loadCounts(); }, 4000); });
          }).catch(function (ex) { out.insertAdjacentHTML('afterbegin', '<p class="error-text">' + esc(ex.message) + '</p>'); });
        };
      }).catch(function (ex) { out.innerHTML = '<p class="error-text">' + esc(ex.message) + '</p>'; });
    };
  }

  function openSourceSheet(id) {
    var s = S.sourceById[id];
    if (!s) return;
    var status = s.is_dead ? '<p class="story-note">This feed stopped updating' + (s.last_error ? ' (' + esc(s.last_error) + ')' : '') + '. Reviving it will try again.</p>' :
      s.last_error ? '<p class="story-note">Last update failed: ' + esc(s.last_error) + '</p>' : '';
    openSheet('<h2 id="sheet-title">' + esc(s.title) + '</h2><p class="muted" style="word-break:break-all">' + esc(s.url) + '</p>' + status +
      '<form id="src-form"><label class="field"><span>Name</span><input id="src-title" value="' + esc(s.title) + '"></label>' +
      '<label class="field"><span>Folder</span><select id="src-folder">' + folderOptions(s.folder_id || 0) + '</select></label>' +
      '<label class="check"><input id="src-full" type="checkbox"' + (s.fetch_full_text ? ' checked' : '') + '><span>Fetch full articles</span></label>' +
      '<div class="sheet-actions"><button class="btn-primary" type="submit">Save</button>' +
      '<button class="btn-quiet" type="button" id="src-refresh">' + icon('i-refresh') + (s.is_dead ? 'Revive' : 'Refresh now') + '</button>' +
      (s.site_url ? '<a class="btn-quiet" href="' + esc(s.site_url) + '" target="_blank" rel="noopener noreferrer">' + icon('i-external') + 'Website</a>' : '') +
      '<button class="btn-quiet btn-danger" type="button" id="src-delete">Unsubscribe</button></div></form>');
    $('#src-form').onsubmit = function (e) {
      e.preventDefault();
      var fid = +$('#src-folder').value || null;
      api('PUT', '/sources/' + id, { title: $('#src-title').value, folder_id: fid, fetch_full_text: $('#src-full').checked }).then(function () {
        closeSheet(); return loadSources();
      }).then(renderTree).catch(function (ex) { toast(ex.message); });
    };
    $('#src-refresh').onclick = function () {
      var p = s.is_dead ? api('PUT', '/sources/' + id, { is_dead: false }) : api('POST', '/sources/refresh', { source_id: id });
      p.then(function () { closeSheet(); toast('Checking ' + s.title + '…'); setTimeout(function () { loadSources().then(renderTree); loadCounts(); if (S.sourceId === id) loadList(true); }, 4000); });
    };
    $('#src-delete').onclick = function () {
      if (!window.confirm('Unsubscribe from ' + s.title + '? Its stories will be removed.')) return;
      api('DELETE', '/sources/' + id).then(function () {
        closeSheet(); toast('Unsubscribed');
        if (S.sourceId === id) select({ view: 'unread' });
        return Promise.all([loadSources(), loadCounts()]);
      }).then(renderTree);
    };
  }

  function openFolderSheet(id) {
    var f = S.folders.filter(function (x) { return x.id === id; })[0];
    if (!f) return;
    openSheet('<h2 id="sheet-title">Folder</h2><form id="fold-form"><label class="field"><span>Name</span><input id="fold-name" value="' + esc(f.name) + '"></label>' +
      '<div class="sheet-actions"><button class="btn-primary" type="submit">Save</button><button class="btn-quiet btn-danger" type="button" id="fold-del">Delete folder</button></div></form>' +
      '<p class="muted">Deleting a folder keeps its feeds; they move to the top level.</p>');
    $('#fold-form').onsubmit = function (e) {
      e.preventDefault();
      api('PUT', '/folders/' + id, { name: $('#fold-name').value }).then(function () { closeSheet(); return loadSources(); }).then(renderTree);
    };
    $('#fold-del').onclick = function () {
      api('DELETE', '/folders/' + id).then(function () { closeSheet(); if (S.folderId === id) select({ view: 'unread' }); return loadSources(); }).then(renderTree);
    };
  }

  function openSettings() {
    openSheet('<h2 id="sheet-title">Settings</h2>' +
      '<form id="newfolder"><label class="field"><span>New folder</span><input id="nf-name" placeholder="e.g. Tech"></label><button class="btn-quiet" type="submit">' + icon('i-folder') + ' Create folder</button></form>' +
      '<label class="field"><span>Import subscriptions (OPML)</span><input id="opml-file" type="file" accept=".opml,.xml,text/xml,application/xml"></label>' +
      '<div class="sheet-actions"><a class="btn-quiet" href="/opml/export" download>Export OPML</a>' +
      '<button class="btn-quiet" type="button" id="set-size">' + icon('i-textsize') + 'Text size</button>' +
      '<button class="btn-quiet" type="button" id="set-keys">Shortcuts</button>' +
      '<button class="btn-quiet btn-danger" type="button" id="set-logout">Sign out</button></div>');
    $('#newfolder').onsubmit = function (e) {
      e.preventDefault();
      var name = $('#nf-name').value.trim();
      if (!name) return;
      api('POST', '/folders', { name: name }).then(function () { $('#nf-name').value = ''; toast('Folder created'); return loadSources(); }).then(renderTree);
    };
    $('#opml-file').onchange = function (e) {
      var file = e.target.files[0];
      if (!file) return;
      readText(file).then(function (xml) { return api('POST', '/opml/import', xml); }).then(function (r) {
        closeSheet();
        toast('Imported ' + r.sources_created + ' feeds' + (r.skipped ? ' (' + r.skipped + ' already subscribed)' : ''));
        return loadSources();
      }).then(renderTree).catch(function (ex) { toast('Import failed: ' + ex.message); });
    };
    $('#set-size').onclick = cycleSize;
    $('#set-keys').onclick = openHelp;
    $('#set-logout').onclick = function () {
      queue = []; store.set('queue', []);
      var clear = window.caches ? caches.keys().then(function (ks) { return Promise.all(ks.map(function (k) { return caches.delete(k); })); }) : Promise.resolve();
      clear.then(function () { return api('POST', '/auth/logout'); }).then(function () { location.href = '/'; });
    };
  }

  function openHelp() {
    var keys = [['j / k', 'Next / previous story'], ['o or Enter', 'Open story'], ['Esc', 'Back / close'], ['m', 'Mark read / unread'],
      ['s', 'Save / unsave'], ['v', 'Open original'], ['f', 'Load full article'], ['l', 'Listen / stop'], ['Space', 'Scroll, then next story'],
      ['Shift + A', 'Mark all as read'], ['r', 'Refresh'], ['/', 'Search'], ['.', 'Ask Noema'], ['z', 'Undo'],
      ['g then u / s / a', 'Unread / Saved / All'], ['?', 'This help']];
    openSheet('<h2 id="sheet-title">Keyboard shortcuts</h2><dl class="shortcuts">' + keys.map(function (k) {
      return '<dt><kbd>' + esc(k[0]) + '</kbd></dt><dd style="margin:0">' + esc(k[1]) + '</dd>';
    }).join('') + '</dl><p class="muted">On touch screens: swipe right to mark read, left to save, press and hold for a preview, pull down to refresh.</p>');
  }

  function readText(file) { // File.text() is Safari 14+
    return new Promise(function (res, rej) {
      var r = new FileReader();
      r.onload = function () { res(r.result); };
      r.onerror = function () { rej(r.error); };
      r.readAsText(file);
    });
  }

  // ── Toast with undo ────────────────────────────────────────────────────
  var toastTimer = 0, undoFn = null;
  function toast(text, undo) {
    var t = $('#toast');
    $('#toast-text').textContent = text;
    undoFn = undo || null;
    $('#toast-undo').hidden = !undo;
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { t.hidden = true; }, undo ? 6000 : 3000);
  }
  function undo() {
    if (!undoFn) return;
    var fn = undoFn; undoFn = null;
    $('#toast').hidden = true;
    fn();
    announce('Undone');
  }

  // ── Speech: read aloud and voice replies ───────────────────────────────
  var synth = window.speechSynthesis;
  var tts = { chunks: [], i: 0, lang: '', active: false, paused: false, keepAlive: 0, onDone: null };
  function speak(chunks, lang, title, label, onDone) {
    stopSpeech();
    if (!synth) { toast('Speech isn’t available in this browser'); return; }
    tts.itemId = 0;
    tts.chunks = chunks.slice(); tts.i = 0; tts.lang = lang || document.documentElement.lang; tts.active = true; tts.paused = false; tts.onDone = onDone || null;
    $('#player-label').textContent = label || 'Listening';
    $('#player-title').textContent = title || '';
    $('#player').hidden = false;
    setPlayIcon();
    // Chrome silently stops long utterances unless nudged.
    if (/Chrome/.test(navigator.userAgent) && !/Edg|OPR/.test(navigator.userAgent)) {
      tts.keepAlive = setInterval(function () { if (synth.speaking && !tts.paused) { synth.pause(); synth.resume(); } }, 10000);
    }
    speakNext();
  }
  function speakNext() {
    if (!tts.active) return;
    if (tts.i >= tts.chunks.length) { var done = tts.onDone; stopSpeech(); if (done) done(); return; }
    var u = new SpeechSynthesisUtterance(tts.chunks[tts.i]);
    if (tts.lang) u.lang = tts.lang;
    u.rate = store.get('speechRate', 1);
    u.onend = function () { if (!tts.active || tts.skipping) return; tts.i++; speakNext(); };
    u.onerror = function (e) { if (e.error !== 'interrupted' && e.error !== 'canceled') { tts.i++; speakNext(); } };
    synth.speak(u);
  }
  function stopSpeech() {
    tts.active = false;
    clearInterval(tts.keepAlive);
    if (synth) synth.cancel();
    var a = $('#audio');
    if (!a.paused) a.pause();
    $('#player').hidden = true;
  }
  function togglePause() {
    var a = $('#audio');
    if (tts.chunks.length === 0 && a.getAttribute('src')) { if (a.paused) a.play(); else a.pause(); setPlayIcon(); return; }
    if (!tts.active) return;
    tts.paused = !tts.paused;
    if (tts.paused) synth.pause(); else synth.resume();
    setPlayIcon();
  }
  function skipChunk() {
    if (!tts.active) return;
    tts.skipping = true;
    synth.cancel();
    tts.skipping = false;
    tts.i++;
    speakNext();
  }
  function setPlayIcon() {
    var paused = tts.paused || (tts.chunks.length === 0 && $('#audio').paused);
    $('use', $('#player-toggle')).setAttribute('href', paused ? '#i-play' : '#i-pause');
    $('#player-toggle').setAttribute('aria-label', paused ? 'Resume' : 'Pause');
  }
  function playAudio(url, title) {
    stopSpeech();
    tts.chunks = [];
    var a = $('#audio');
    a.src = url;
    $('#player-label').textContent = 'Playing';
    $('#player-title').textContent = title || '';
    $('#player').hidden = false;
    a.play().catch(function () {});
    setPlayIcon();
  }
  function listen(it) {
    if (tts.active && tts.itemId === it.id) { stopSpeech(); return; }
    var enc = (it.enclosures || []).filter(function (e) { return /^audio\//.test(e.type || ''); })[0];
    if (enc) { playAudio(enc.url, it.title); return; }
    api('GET', '/items/' + it.id + '/speech').then(function (r) {
      speak(r.chunks, r.lang || it.lang, it.title, 'Listening');
      tts.itemId = it.id;
    }).catch(function (e) { toast(e.message); });
  }

  // Voice agent: speech recognition where available, typing everywhere.
  var Recognition = window.SpeechRecognition || window.webkitSpeechRecognition;
  var rec = null, listening = false;
  function openVoice() {
    $('#voice').hidden = false;
    $('#voice-fab').setAttribute('aria-expanded', 'true');
    if (!$('#voice-log').children.length) addMsg('noema', 'Try “What’s new?”, “What did I miss in Tech today?”, or “Read the first one”.');
    if (Recognition) startListening(); else $('#voice-text').focus();
  }
  function closeVoice() {
    stopListening();
    $('#voice').hidden = true;
    $('#voice-fab').setAttribute('aria-expanded', 'false');
    $('#voice-fab').focus();
  }
  function startListening() {
    if (!Recognition || listening) return;
    stopSpeech();
    rec = new Recognition();
    rec.lang = navigator.language || 'en-US';
    rec.interimResults = true;
    rec.maxAlternatives = 1;
    listening = true;
    $('#voice-mic').classList.add('listening');
    $('#voice-fab').classList.add('listening');
    $('#voice-mic').setAttribute('aria-label', 'Stop listening');
    rec.onresult = function (e) {
      var text = '', final = false;
      for (var i = e.resultIndex; i < e.results.length; i++) { text += e.results[i][0].transcript; if (e.results[i].isFinal) final = true; }
      $('#voice-text').value = text;
      if (final) { stopListening(); ask(text); }
    };
    rec.onerror = function (e) { stopListening(); if (e.error === 'not-allowed') toast('Microphone permission was denied. You can type instead.'); };
    rec.onend = stopListening;
    try { rec.start(); } catch (e) { stopListening(); }
  }
  function stopListening() {
    listening = false;
    $('#voice-mic').classList.remove('listening');
    $('#voice-fab').classList.remove('listening');
    $('#voice-mic').setAttribute('aria-label', 'Speak');
    if (rec) { try { rec.stop(); } catch (e) {} rec = null; }
  }
  function addMsg(who, html, isHTML) {
    var div = document.createElement('div');
    div.className = 'msg ' + who;
    div.innerHTML = '<span class="bubble">' + (isHTML ? html : esc(html)) + '</span>';
    $('#voice-log').appendChild(div);
    $('#voice-log').scrollTop = 1e6;
    return div;
  }
  function ask(text) {
    text = (text || '').trim();
    if (!text) return;
    $('#voice-text').value = '';
    addMsg('you', text);
    api('POST', '/ask', { text: text, lang: navigator.language, context: S.voiceCtx }).then(function (r) {
      S.voiceCtx = r.context;
      var html = esc(r.text || '');
      if (r.items && r.items.length) {
        html = esc(r.speak[0] || '') + '<ol>' + r.items.map(function (it) {
          S.byId[it.id] = S.byId[it.id] || it;
          return '<li><a href="/item/' + it.id + '" data-open="' + it.id + '">' + esc(it.title) + '</a> <span class="muted">— ' + esc(it.source_title) + '</span></li>';
        }).join('') + '</ol>';
      } else if (r.item && r.command.intent === 'read') {
        html = 'Reading <a href="/item/' + r.item.id + '" data-open="' + r.item.id + '">' + esc(r.item.title) + '</a>';
      }
      addMsg('noema', html, true);
      if (r.undo_ids && r.undo_ids.length) {
        toast(r.text, function () { api('POST', '/items/mark-read', { ids: r.undo_ids, read: false, at: now() }).then(function () { applyRead(r.undo_ids, false); loadCounts(); }); });
        applyRead(r.undo_ids, true);
      }
      if (r.item) {
        var local = S.byId[r.item.id];
        if (local && (local.is_read !== r.item.is_read || local.is_starred !== r.item.is_starred)) { loadCounts(); }
      }
      var act = r.action || {};
      if (act.type === 'stop') { stopSpeech(); return; }
      if (act.type === 'open' && act.item_id) openItem(act.item_id, true);
      if (act.type === 'play' && act.url) { playAudio(act.url, r.item ? r.item.title : ''); return; }
      if (r.speak && r.speak.length) speak(r.speak, (r.command.intent === 'read' && r.item && r.item.lang) || navigator.language, r.item ? r.item.title : 'Noema', r.command.intent === 'read' ? 'Listening' : 'Noema');
      if (['read', 'star', 'unstar', 'mark_read', 'mark_unread', 'mark_all'].indexOf(r.command.intent) >= 0) {
        loadCounts();
        if (r.item && S.byId[r.item.id]) {
          var li = S.byId[r.item.id];
          if (r.command.intent === 'read' || r.command.intent === 'mark_read') li.is_read = true;
          if (r.command.intent === 'mark_unread') li.is_read = false;
          if (r.command.intent === 'star') li.is_starred = true;
          if (r.command.intent === 'unstar') li.is_starred = false;
          refreshRow(li);
        }
      }
    }).catch(function (e) { addMsg('noema', 'Sorry — ' + e.message); });
  }

  // ── Drawer ─────────────────────────────────────────────────────────────
  function openDrawer() {
    $('#app').classList.add('drawer');
    $('#scrim').hidden = false;
    $('#open-sidebar').setAttribute('aria-expanded', 'true');
    var cur = $('#sidebar [aria-current="page"]') || $('#sidebar a');
    if (cur) cur.focus();
  }
  function closeDrawer() {
    if (!$('#app').classList.contains('drawer')) return;
    $('#app').classList.remove('drawer');
    $('#scrim').hidden = true;
    $('#open-sidebar').setAttribute('aria-expanded', 'false');
  }

  // ── Wiring ─────────────────────────────────────────────────────────────
  function typing(e) {
    var t = e.target;
    return t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT' || t.isContentEditable);
  }
  function bind() {
    $('#open-sidebar').onclick = openDrawer;
    $('#close-sidebar').onclick = closeDrawer;
    $('#scrim').onclick = closeDrawer;
    $('#refresh').onclick = refresh;
    $('#mark-all').onclick = markAll;
    $('#add-feed').onclick = openAddFeed;
    $('#open-settings').onclick = openSettings;
    $('#back').onclick = function () { closeReader(true); };
    $('#act-read').onclick = function () { var it = S.byId[S.openId]; if (it) toggleRead(it); };
    $('#act-star').onclick = function () { var it = S.byId[S.openId]; if (it) toggleStar(it); };
    $('#act-listen').onclick = function () { var it = S.byId[S.openId]; if (it) listen(it); };
    $('#act-full').onclick = loadFullText;
    $('#act-size').onclick = cycleSize;
    $('#toast-undo').onclick = undo;
    $('#sheet-scrim').onclick = closeSheet;
    $('#player-toggle').onclick = togglePause;
    $('#player-next').onclick = skipChunk;
    $('#player-stop').onclick = stopSpeech;
    $('#audio').onplay = $('#audio').onpause = setPlayIcon;
    $('#voice-fab').onclick = function () { if ($('#voice').hidden) openVoice(); else closeVoice(); };
    $('#voice-close').onclick = closeVoice;
    $('#voice-mic').onclick = function () { if (listening) stopListening(); else startListening(); };
    $('#voice-mic').hidden = !Recognition;
    $('#voice-form').onsubmit = function (e) { e.preventDefault(); ask($('#voice-text').value); };
    $('#voice-log').onclick = function (e) {
      var a = e.target.closest && e.target.closest('[data-open]');
      if (a) { e.preventDefault(); openItem(+a.getAttribute('data-open'), true); }
    };
    $('#search-toggle').onclick = function () {
      var bar = $('#searchbar');
      bar.hidden = !bar.hidden;
      if (!bar.hidden) $('#search').focus();
      else if (S.q) { S.q = ''; loadList(true); renderTree(); }
    };
    var searchTimer = 0;
    $('#search').addEventListener('input', function () {
      clearTimeout(searchTimer);
      searchTimer = setTimeout(function () { S.q = $('#search').value.trim(); loadList(true); renderTree(); }, 250);
    });
    $('#searchbar').onsubmit = function (e) { e.preventDefault(); S.q = $('#search').value.trim(); loadList(true); };

    // Sidebar navigation (event delegation; links keep real hrefs for new tabs).
    $('#sidebar').addEventListener('click', function (e) {
      var t = e.target.closest && e.target.closest('[data-toggle], [data-view], [data-source], [data-folder]');
      if (!t) return;
      if (e.metaKey || e.ctrlKey || e.shiftKey) return;
      e.preventDefault();
      if (t.hasAttribute('data-toggle')) {
        e.stopPropagation();
        var id = +t.getAttribute('data-toggle');
        S.expanded[id] = S.expanded[id] === false;
        store.set('expanded', S.expanded);
        renderTree();
        var btn = document.querySelector('[data-toggle="' + id + '"]');
        if (btn) btn.focus();
        return;
      }
      if (t.hasAttribute('data-view')) select({ view: t.getAttribute('data-view') });
      else if (t.hasAttribute('data-source')) select({ view: 'all', sourceId: +t.getAttribute('data-source') });
      else select({ view: 'unread', folderId: +t.getAttribute('data-folder') });
    });
    $('#sidebar').addEventListener('contextmenu', function (e) {
      var t = e.target.closest && e.target.closest('[data-source], [data-folder]');
      if (!t) return;
      e.preventDefault();
      if (t.hasAttribute('data-source')) openSourceSheet(+t.getAttribute('data-source'));
      else openFolderSheet(+t.getAttribute('data-folder'));
    });
    longPress($('#sidebar'), '[data-source], [data-folder]', function (t) {
      if (t.hasAttribute('data-source')) openSourceSheet(+t.getAttribute('data-source'));
      else openFolderSheet(+t.getAttribute('data-folder'));
    });
    $('#story-body').addEventListener('click', function (e) {
      var a = e.target.closest && e.target.closest('[data-source]');
      if (a && !e.metaKey && !e.ctrlKey) { e.preventDefault(); select({ view: 'all', sourceId: +a.getAttribute('data-source') }); closeReader(false); }
    });

    // Infinite scroll.
    if ('IntersectionObserver' in window) {
      new IntersectionObserver(function (entries) {
        if (entries[0].isIntersecting && S.items.length) loadList(false);
      }, { root: $('#listscroll'), rootMargin: '600px' }).observe($('#list-foot'));
    } else {
      $('#listscroll').addEventListener('scroll', function () {
        var el = $('#listscroll');
        if (el.scrollTop + el.clientHeight > el.scrollHeight - 800) loadList(false);
      });
    }
    bindPullToRefresh();

    document.addEventListener('keydown', onKey);
    // Keep the sheet's focus inside it (it is modal).
    $('#sheet').addEventListener('keydown', function (e) {
      if (e.key !== 'Tab') return;
      var f = Array.prototype.filter.call($('#sheet').querySelectorAll('a[href], button, input, select, textarea'), function (x) { return !x.disabled && x.offsetParent !== null; });
      if (!f.length) return;
      var first = f[0], last = f[f.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    });

    // Keep counts fresh; offer new stories rather than reshuffling the list.
    setInterval(function () {
      if (document.hidden) return;
      loadCounts().then(function (fresh) {
        if (fresh && !S.q) toast('New stories', function () { loadList(true); });
      }).catch(function () {});
    }, 180000);
    document.addEventListener('visibilitychange', function () { if (!document.hidden) { loadCounts().catch(function () {}); flush(); } });
  }

  function longPress(root, selector, fn) {
    var timer = 0, fired = false, sx = 0, sy = 0;
    root.addEventListener('pointerdown', function (e) {
      var t = e.target.closest && e.target.closest(selector);
      if (!t || e.pointerType === 'mouse') return;
      fired = false; sx = e.clientX; sy = e.clientY;
      timer = setTimeout(function () { fired = true; fn(t); }, 500);
    });
    root.addEventListener('pointermove', function (e) { if (Math.abs(e.clientX - sx) > 8 || Math.abs(e.clientY - sy) > 8) clearTimeout(timer); });
    root.addEventListener('pointerup', function () { clearTimeout(timer); });
    root.addEventListener('click', function (e) { if (fired) { e.preventDefault(); e.stopPropagation(); fired = false; } }, true);
  }

  function onKey(e) {
    if (e.key === 'Escape') {
      if (sheetOpen()) { closeSheet(); return; }
      if (!$('#voice').hidden) { closeVoice(); return; }
      if ($('#app').classList.contains('drawer')) { closeDrawer(); $('#open-sidebar').focus(); return; }
      if (document.activeElement === $('#search')) { $('#search').blur(); return; }
      if (S.openId && (narrow() || $('#app').classList.contains('reading'))) { closeReader(true); return; }
      return;
    }
    if (typing(e) || e.metaKey || e.ctrlKey || e.altKey || sheetOpen() || $('#app').hidden) return;
    if (S.gPrefix) {
      S.gPrefix = false;
      var v = { u: 'unread', s: 'starred', a: 'all' }[e.key];
      if (v) { e.preventDefault(); select({ view: v }); }
      return;
    }
    var it = current();
    switch (e.key) {
      case 'j': case 'ArrowDown': if (e.key === 'ArrowDown' && document.activeElement && !document.activeElement.closest('.list')) return; e.preventDefault(); move(1, e.key === 'j'); break;
      case 'k': case 'ArrowUp': if (e.key === 'ArrowUp' && document.activeElement && !document.activeElement.closest('.list')) return; e.preventDefault(); move(-1, e.key === 'k'); break;
      case 'o': case 'Enter': if (S.items[S.sel] && (e.key === 'o' || document.activeElement === document.body)) { e.preventDefault(); openItem(S.items[S.sel].id, true); } break;
      case 'm': if (it) toggleRead(it); break;
      case 's': if (it) toggleStar(it); break;
      case 'v': if (it && it.url) window.open(it.url, '_blank', 'noopener'); break;
      case 'f': if (S.openId) loadFullText(); break;
      case 'l': if (it) listen(it); break;
      case 'r': refresh(); break;
      case 'A': markAll(); break;
      case 'z': undo(); break;
      case '/': e.preventDefault(); $('#searchbar').hidden = false; $('#search').focus(); break;
      case '.': e.preventDefault(); if ($('#voice').hidden) openVoice(); else closeVoice(); break;
      case '?': openHelp(); break;
      case 'g': S.gPrefix = true; setTimeout(function () { S.gPrefix = false; }, 1200); break;
      case ' ':
        if (S.openId) {
          var rd = $('#reader');
          if (rd.scrollTop + rd.clientHeight >= rd.scrollHeight - 4) { e.preventDefault(); move(1, true); }
          else if (document.activeElement !== rd) { e.preventDefault(); rd.scrollBy({ top: rd.clientHeight * 0.85, behavior: reducedMotion ? 'auto' : 'smooth' }); }
        }
        break;
    }
  }

  boot();
})();
