// Zero-dependency static server for local development (no build step needed):
//   API_URL=http://localhost:8080 PORT=3000 npm run dev
import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { extname, join, normalize } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('.', import.meta.url));
const port = Number(process.env.PORT || 3000);
const apiUrl = process.env.API_URL || 'http://localhost:8080';
const types = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.ico': 'image/x-icon' };

createServer(async (req, res) => {
  const path = decodeURIComponent(new URL(req.url, 'http://x').pathname);
  if (path === '/config.js') {
    res.writeHead(200, { 'Content-Type': 'text/javascript', 'Cache-Control': 'no-store' });
    return res.end(`window.APP_CONFIG = ${JSON.stringify({ API_URL: apiUrl })};\n`);
  }
  const rel = normalize(path).replace(/^(\.\.[/\\])+/, '');
  const candidates = [join(root, 'public', rel), join(root, rel.startsWith('/src/') ? rel : '/nonexistent')];
  for (const file of candidates) {
    if (!file.startsWith(root) || !extname(file)) continue;
    try {
      const body = await readFile(file);
      res.writeHead(200, { 'Content-Type': types[extname(file)] || 'application/octet-stream' });
      return res.end(body);
    } catch { /* try next */ }
  }
  // single-page-app fallback: /login, /products/3, /admin/orders ...
  res.writeHead(200, { 'Content-Type': 'text/html' });
  res.end(await readFile(join(root, 'public', 'index.html')));
}).listen(port, () => console.log(`serving on http://localhost:${port}  (API_URL=${apiUrl})`));
