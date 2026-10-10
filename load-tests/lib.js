// Shared helpers for the k6 scripts. Every request carries X-LoadTest / X-VU so the load balancer can
// compute real load-test metrics for the dashboard from the traffic it actually sees.
import http from 'k6/http';
import { check, group } from 'k6';
import { uuidv4 } from 'https://jslib.k6.io/k6-utils/1.4.0/index.js';

export const BASE = __ENV.BASE_URL || 'http://localhost:8080';

export function params(testName, extra = {}) {
  return {
    headers: {
      'Content-Type': 'application/json',
      'X-Requested-With': 'k6',
      'X-LoadTest': testName,
      'X-VU': String(__VU),
      // Spread virtual users over many "client IPs" so ip_hash has something to hash (trusted from private peers).
      'X-Forwarded-For': `10.${(__VU >> 8) & 255}.${__VU & 255}.${(__ITER % 250) + 1}`,
      ...(extra.headers || {}),
    },
    tags: extra.tags || {},
  };
}

// Registers (first iteration of each VU) and logs in. The session cookie lives in the VU's cookie jar.
export function ensureSession(testName) {
  if (__ITER > 0 && globalThis.__loggedIn === __VU) return;
  const email = `k6-${__ENV.RUN_ID || 'run'}-vu${__VU}@example.com`;
  const body = JSON.stringify({ name: `K6 User ${__VU}`, email, password: 'k6-password-123' });
  let res = http.post(`${BASE}/api/auth/login`, body, params(testName, { tags: { name: 'login' } }));
  if (res.status !== 200) {
    http.post(`${BASE}/api/auth/register`, body, params(testName, { tags: { name: 'register' } }));
    res = http.post(`${BASE}/api/auth/login`, body, params(testName, { tags: { name: 'login' } }));
  }
  check(res, { 'login 200': (r) => r.status === 200 });
  globalThis.__loggedIn = __VU;
}

// One realistic shopper journey: browse -> view -> cart -> checkout -> order -> history.
export function shopperJourney(testName, { order = true } = {}) {
  let products = [];
  group('browse', () => {
    const r = http.get(`${BASE}/api/products`, params(testName, { tags: { name: 'products' } }));
    check(r, { 'products 200': (x) => x.status === 200 });
    try { products = r.json(); } catch (e) { /* ignore */ }
  });
  if (!products.length) return;
  const p = products[Math.floor(Math.random() * products.length)];
  group('view', () => {
    check(http.get(`${BASE}/api/products/${p.id}`, params(testName, { tags: { name: 'product' } })), { 'product 200': (x) => x.status === 200 });
  });
  group('cart', () => {
    check(http.post(`${BASE}/api/cart`, JSON.stringify({ product_id: p.id, quantity: 1 }), params(testName, { tags: { name: 'add_to_cart' } })), { 'add to cart 201': (x) => x.status === 201 });
    check(http.get(`${BASE}/api/cart`, params(testName, { tags: { name: 'view_cart' } })), { 'view cart 200': (x) => x.status === 200 });
  });
  if (!order) return;
  group('checkout', () => {
    check(http.post(`${BASE}/api/checkout`, null, params(testName, { tags: { name: 'checkout' } })), { 'checkout 200': (x) => x.status === 200 });
    const o = http.post(`${BASE}/api/orders`, JSON.stringify({ address: 'k6 load test street 1' }),
      params(testName, { headers: { 'Idempotency-Key': uuidv4() }, tags: { name: 'place_order' } }));
    check(o, { 'order created (201)': (x) => x.status === 201 });
    check(http.get(`${BASE}/api/orders`, params(testName, { tags: { name: 'order_history' } })), { 'orders 200': (x) => x.status === 200 });
  });
}
