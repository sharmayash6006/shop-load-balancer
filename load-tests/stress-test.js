// Stress: keep raising the arrival rate until something breaks. Shows how the LB behaves at saturation.
import http from 'k6/http';
import { check } from 'k6';
import { BASE, params } from './lib.js';

export const options = {
  scenarios: {
    stress: {
      executor: 'ramping-arrival-rate', startRate: 100, timeUnit: '1s', preAllocatedVUs: 200, maxVUs: 2000,
      stages: [{ target: 500, duration: '30s' }, { target: 1500, duration: '30s' }, { target: 3000, duration: '30s' }, { target: 0, duration: '15s' }],
    },
  },
};

export default function () {
  check(http.get(`${BASE}/api/products`, params('stress', { tags: { name: 'products' } })), { 'status < 500': (r) => r.status < 500 });
}
