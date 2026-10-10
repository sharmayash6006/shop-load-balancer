// Fixed number of requests, for "100 / 1,000 / 10,000 / 50,000 requests" runs:
//   REQUESTS=10000 VUS=100 docker compose run --rm k6 run /tests/requests-test.js
import http from 'k6/http';
import { check } from 'k6';
import { BASE, params, ensureSession } from './lib.js';

export const options = {
  scenarios: { fixed: { executor: 'shared-iterations', vus: Number(__ENV.VUS || 100), iterations: Number(__ENV.REQUESTS || 10000), maxDuration: '30m' } },
};

export default function () {
  ensureSession('requests');
  const r = Math.random();
  if (r < 0.6) check(http.get(`${BASE}/api/products`, params('requests', { tags: { name: 'products' } })), { 'GET 200': (x) => x.status === 200 });
  else if (r < 0.8) check(http.get(`${BASE}/api/cart`, params('requests', { tags: { name: 'cart' } })), { 'cart 200': (x) => x.status === 200 });
  else if (r < 0.95) check(http.post(`${BASE}/api/cart`, JSON.stringify({ product_id: 1 + Math.floor(Math.random() * 8), quantity: 1 }), params('requests', { tags: { name: 'add_to_cart' } })), { 'POST 201': (x) => x.status === 201 });
  else check(http.post(`${BASE}/api/auth/login`, JSON.stringify({ email: `k6-${__ENV.RUN_ID || 'run'}-vu${__VU}@example.com`, password: 'k6-password-123' }), params('requests', { tags: { name: 'login' } })), { 'login 200': (x) => x.status === 200 });
}
