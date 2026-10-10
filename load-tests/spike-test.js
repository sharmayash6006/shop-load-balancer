// Spike: sudden jump from 20 to 800 virtual users and back.
import http from 'k6/http';
import { check, sleep } from 'k6';
import { BASE, params } from './lib.js';

export const options = {
  stages: [{ duration: '15s', target: 20 }, { duration: '5s', target: 800 }, { duration: '30s', target: 800 }, { duration: '5s', target: 20 }, { duration: '20s', target: 20 }, { duration: '10s', target: 0 }],
};

export default function () {
  check(http.get(`${BASE}/api/products`, params('spike', { tags: { name: 'products' } })), { 'status < 500': (r) => r.status < 500 });
  sleep(0.1);
}
