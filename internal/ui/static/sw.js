// Phase 42 — minimal service worker.
//
// Goal: install-to-home-screen on iOS Safari + Android Chrome,
// plus a tiny offline-shell so a network blip doesn't return a
// browser error page. Data fetches (HTMX swaps, dashboard polls)
// fall through to network and FAIL fast — we don't pretend to
// have offline data; the offline shell exists only to keep the
// last-rendered page paintable while reconnect is in progress.
//
// Caching strategy:
//   - Static assets (/ui/static/*): cache-first, revalidate.
//   - Page navigations: network-first, fall back to the cached
//     last-rendered version of the same path.
//   - Everything else (API, /metrics, etc.): network-only.

const VERSION = 'v1';
const STATIC_CACHE = `vornik-static-${VERSION}`;
const PAGES_CACHE = `vornik-pages-${VERSION}`;

const STATIC_PRECACHE = [
  '/ui/',
  '/ui/static/htmx.min.js',
  '/ui/static/htmx-ext-sse.js',
  '/ui/static/manifest.webmanifest',
  '/ui/static/icon.svg',
];

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(STATIC_CACHE).then((cache) =>
      // Pre-cache best-effort; missing assets don't block install.
      Promise.allSettled(STATIC_PRECACHE.map((url) => cache.add(url)))
    )
  );
  self.skipWaiting();
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(
        keys
          .filter((k) => k !== STATIC_CACHE && k !== PAGES_CACHE)
          .map((k) => caches.delete(k))
      )
    )
  );
  self.clients.claim();
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;

  // Static assets: cache-first.
  if (url.pathname.startsWith('/ui/static/')) {
    event.respondWith(
      caches.match(req).then((hit) => hit || fetch(req).then((res) => {
        const copy = res.clone();
        caches.open(STATIC_CACHE).then((c) => c.put(req, copy));
        return res;
      }))
    );
    return;
  }

  // Page navigations: network-first, cache as fallback. HTMX
  // partial swaps (`HX-Request: true`) skip the cache to avoid
  // confusing the swap target with a stale outerHTML.
  if (req.mode === 'navigate' && url.pathname.startsWith('/ui/')) {
    event.respondWith(
      fetch(req)
        .then((res) => {
          const copy = res.clone();
          caches.open(PAGES_CACHE).then((c) => c.put(req, copy));
          return res;
        })
        .catch(() => caches.match(req))
    );
    return;
  }

  // Everything else: network-only. Don't cache /api/v1/, /metrics,
  // /readyz, HTMX partial swaps. Failing fast surfaces the network
  // error to the user; pretending to have data would mislead.
});
