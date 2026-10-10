// Realistic shopper flow. Run:  docker compose run --rm k6 run /tests/ecommerce-test.js
//   VUS=100 DURATION=2m docker compose run --rm k6 run /tests/ecommerce-test.js
import { sleep } from 'k6';
import { ensureSession, shopperJourney } from './lib.js';

export const options = {
  scenarios: {
    shoppers: { executor: 'constant-vus', vus: Number(__ENV.VUS || 50), duration: __ENV.DURATION || '60s' },
  },
  thresholds: { http_req_failed: ['rate<0.05'], http_req_duration: ['p(95)<1500'] },
};

export default function () {
  ensureSession('ecommerce');
  shopperJourney('ecommerce');
  sleep(Math.random() * 0.5);
}
