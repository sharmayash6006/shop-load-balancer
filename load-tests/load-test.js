// Ramp through 10 -> 50 -> 100 -> 500 -> 1000 virtual users (read-heavy mix with some cart/order traffic).
//   docker compose run --rm k6 run /tests/load-test.js
// Set STAGES=small to stop at 100 VUs on a laptop.
import http from 'k6/http';
import { check, sleep } from 'k6';
import { BASE, params, ensureSession, shopperJourney } from './lib.js';

const big = (__ENV.STAGES || '') !== 'small';
export const options = {
  stages: [
    { duration: '20s', target: 10 }, { duration: '20s', target: 50 }, { duration: '30s', target: 100 },
    ...(big ? [{ duration: '40s', target: 500 }, { duration: '40s', target: 1000 }] : []),
    { duration: '20s', target: 0 },
  ],
  thresholds: { http_req_failed: ['rate<0.10'] },
};

export default function () {
  ensureSession('load');
  const r = Math.random();
  if (r < 0.7) {
    check(http.get(`${BASE}/api/products`, params('load', { tags: { name: 'products' } })), { 'products 200': (x) => x.status === 200 });
  } else if (r < 0.9) {
    shopperJourney('load', { order: false });
  } else {
    shopperJourney('load');
  }
  sleep(0.2 + Math.random() * 0.5);
}
