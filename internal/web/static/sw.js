/* Noema service worker: the app shell works offline, and the last-seen
   timelines and stories stay readable without a connection. State changes are
   queued by the page itself (localStorage) and replayed when back online. */
var VERSION = 'noema-v1';
var SHELL = ['/', '/app.css', '/app.js', '/manifest.webmanifest', '/icon.svg', '/icon-192.png', '/favicon.png'];

self.addEventListener('install', function (e) {
  e.waitUntil(caches.open(VERSION).then(function (c) { return c.addAll(SHELL); }).then(function () { return self.skipWaiting(); }));
});

self.addEventListener('activate', function (e) {
  e.waitUntil(caches.keys().then(function (keys) {
    return Promise.all(keys.filter(function (k) { return k !== VERSION && k !== VERSION + '-data' && k.indexOf('noema-') === 0; }).map(function (k) { return caches.delete(k); }));
  }).then(function () { return self.clients.claim(); }));
});

function isReadAPI(url) {
  return url.pathname === '/items' || /^\/items\/\d+$/.test(url.pathname) || url.pathname === '/counts' ||
    url.pathname === '/sources' || url.pathname === '/folders';
}

self.addEventListener('fetch', function (e) {
  var req = e.request;
  if (req.method !== 'GET') return;
  var url = new URL(req.url);
  if (url.origin !== location.origin) return; // images etc. go straight to the network

  // Reads: network first, fall back to the last good response.
  if (isReadAPI(url)) {
    e.respondWith(fetch(req).then(function (res) {
      if (res.ok) { var copy = res.clone(); caches.open(VERSION + '-data').then(function (c) { c.put(req, copy); }); }
      return res;
    }).catch(function () {
      return caches.match(req).then(function (hit) {
        return hit || new Response(JSON.stringify({ error: 'offline' }), { status: 503, headers: { 'Content-Type': 'application/json' } });
      });
    }));
    return;
  }
  // Client routes (/item/123, /?view=…) are the shell.
  if (req.mode === 'navigate') {
    e.respondWith(fetch(req).catch(function () { return caches.match('/'); }));
    return;
  }
  // Shell assets: stale-while-revalidate.
  if (SHELL.indexOf(url.pathname) >= 0) {
    e.respondWith(caches.open(VERSION).then(function (c) {
      return c.match(req).then(function (hit) {
        var net = fetch(req).then(function (res) { if (res.ok) c.put(req, res.clone()); return res; });
        return hit || net;
      });
    }));
  }
});
