'use strict';
// ShopLB single-page app. Every call below goes to API_URL (the load balancer). The site never talks to a backend server.
const $ = (s) => document.querySelector(s);
// The ONLY thing this site knows about the backend side: the API / load balancer entry point.
const API = ((window.APP_CONFIG && window.APP_CONFIG.API_URL) || '').replace(/\/+$/, '');
const app = $('#app');
const state = { user: null, cart: { count: 0, items: [], total: 0 }, checkoutKey: null };

const esc = (v) => String(v ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const money = (n) => '₹' + Number(n).toLocaleString('en-IN', { maximumFractionDigits: 2 });
const uuid = () => (crypto.randomUUID ? crypto.randomUUID() : 'k-' + Date.now() + '-' + Math.random().toString(16).slice(2));

function toast(msg) {
  const t = $('#toast');
  t.textContent = msg;
  t.classList.add('show');
  clearTimeout(toast.t);
  toast.t = setTimeout(() => t.classList.remove('show'), 2400);
}

async function api(method, path, body, headers = {}) {
  const opts = { method, credentials: 'include', headers: { 'X-Requested-With': 'shoplb', ...headers } };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(API + path, opts);
  } catch (e) {
    throw Object.assign(new Error('Network error — is the load balancer running?'), { status: 0 });
  }
  const served = res.headers.get('X-Served-By');
  if (served) $('#served').textContent = `Last request served by ${served} (${res.headers.get('X-Request-ID') || ''})`;
  let data = null;
  try { data = await res.json(); } catch (e) { /* empty body */ }
  if (!res.ok) throw Object.assign(new Error((data && data.error) || `Request failed (${res.status})`), { status: res.status, data });
  return data;
}

// ---------- session / cart ----------
async function loadSession() {
  try { state.user = await api('GET', '/api/auth/me'); } catch (e) { state.user = null; }
  await refreshCart();
  renderNav();
}
async function refreshCart() {
  if (!state.user) { state.cart = { count: 0, items: [], total: 0 }; return; }
  try { state.cart = await api('GET', '/api/cart'); } catch (e) { /* keep old */ }
}
function renderNav() {
  const p = location.pathname;
  const a = (href, label) => `<a href="${href}" data-link class="${p.startsWith(href) ? 'on' : ''}">${label}</a>`;
  $('#nav').innerHTML = a('/products', 'Products') +
    (state.user
      ? a('/cart', `Cart<span class="badge">${state.cart.count}</span>`) + a('/orders', 'Orders') + a('/profile', esc(state.user.name))
      : a('/login', 'Login') + a('/register', 'Register'));
}

// ---------- router ----------
const routes = [
  [/^\/(home)?$/, home], [/^\/products$/, products], [/^\/products\/(\d+)$/, productDetail],
  [/^\/login$/, loginPage], [/^\/register$/, registerPage], [/^\/cart$/, cartPage], [/^\/checkout$/, checkoutPage],
  [/^\/orders$/, ordersPage], [/^\/orders\/([\w-]+)$/, orderDetail], [/^\/profile$/, profilePage],
];
const protectedPaths = /^\/(cart|checkout|orders|profile)/;

function go(path, replace) {
  if (replace) history.replaceState(null, '', path); else history.pushState(null, '', path);
  route();
}
async function route() {
  const path = location.pathname;
  if (protectedPaths.test(path) && !state.user) return go('/login?next=' + encodeURIComponent(path), true);
  for (const [re, fn] of routes) {
    const m = path.match(re);
    if (m) {
      app.innerHTML = '<p class="muted">Loading…</p>';
      try { await fn(...m.slice(1)); } catch (e) { app.innerHTML = `<div class="card"><p class="err">${esc(e.message)}</p></div>`; }
      renderNav();
      window.scrollTo(0, 0);
      return;
    }
  }
  app.innerHTML = '<h1>Not found</h1><a href="/home" data-link>Back to home</a>';
}
document.addEventListener('click', (e) => {
  const a = e.target.closest('a[data-link]');
  if (a && !e.metaKey && !e.ctrlKey) { e.preventDefault(); go(a.getAttribute('href')); }
});
window.addEventListener('popstate', route);

// ---------- components ----------
const productCard = (p) => `
  <div class="card product">
    <div class="img">${esc(p.image)}</div>
    <h3><a href="/products/${p.id}" data-link>${esc(p.name)}</a></h3>
    <div class="muted">${esc(p.category)}</div>
    <div class="row spread"><span class="price">${money(p.price)}</span>
    <button class="btn" data-add="${p.id}">Add to cart</button></div>
  </div>`;

async function addToCart(id, qty = 1) {
  if (!state.user) return go('/login?next=' + encodeURIComponent(location.pathname));
  try {
    state.cart = await api('POST', '/api/cart', { product_id: Number(id), quantity: qty });
    renderNav();
    toast('Added to cart');
  } catch (e) { toast(e.message); }
}
document.addEventListener('click', (e) => {
  const b = e.target.closest('[data-add]');
  if (b) addToCart(b.dataset.add, 1);
});

// ---------- pages ----------
async function home() {
  const list = await api('GET', '/api/products');
  app.innerHTML = `<section class="hero"><h1>Gadgets, delivered through a load balancer</h1>
    <p>Browse, add to cart and place real orders. Every request is routed, logged and traced by the Layer 7 load balancer.</p>
    <a class="btn" style="background:#fff;color:#4f46e5" href="/products" data-link>Shop now</a></section>
    <h2>Featured</h2><div class="grid">${list.slice(0, 4).map(productCard).join('')}</div>`;
}

async function products() {
  const params = new URLSearchParams(location.search);
  const q = params.get('q') || '', cat = params.get('category') || '';
  const list = await api('GET', '/api/products?' + new URLSearchParams({ q, category: cat }));
  const cats = [...new Set((await api('GET', '/api/products')).map((p) => p.category))];
  app.innerHTML = `<h1>Products</h1>
    <form class="row" id="filters"><input name="q" placeholder="Search products" value="${esc(q)}" style="max-width:280px">
    <select name="category" style="max-width:200px"><option value="">All categories</option>${cats.map((c) => `<option ${c === cat ? 'selected' : ''}>${esc(c)}</option>`).join('')}</select>
    <button class="btn">Filter</button></form><br>
    <div class="grid">${list.map(productCard).join('') || '<p class="muted">No products match.</p>'}</div>`;
  $('#filters').onsubmit = (e) => {
    e.preventDefault();
    const f = new FormData(e.target);
    go('/products?' + new URLSearchParams({ q: f.get('q'), category: f.get('category') }));
  };
}

async function productDetail(id) {
  const p = await api('GET', '/api/products/' + id);
  app.innerHTML = `<div class="detail"><div class="img">${esc(p.image)}</div><div>
    <div class="muted">${esc(p.category)}</div><h1>${esc(p.name)}</h1><p>${esc(p.description)}</p>
    <div class="price" style="font-size:28px">${money(p.price)}</div><p class="muted">${p.stock > 0 ? 'In stock' : 'Out of stock'}</p>
    <div class="row"><input id="qty" type="number" min="1" max="99" value="1" style="width:90px">
    <button class="btn" id="add">Add to cart</button><button class="btn ghost" id="buy">Buy now</button></div>
    <p class="err" id="err"></p></div></div>`;
  const qty = () => Math.max(1, Math.min(99, parseInt($('#qty').value, 10) || 1));
  $('#add').onclick = () => addToCart(p.id, qty());
  $('#buy').onclick = async (ev) => {
    if (!state.user) return go('/login?next=' + encodeURIComponent(location.pathname));
    ev.target.disabled = true;
    try {
      const order = await api('POST', '/api/orders', { address: 'Buy-now order', items: [{ product_id: p.id, quantity: qty() }] }, { 'Idempotency-Key': uuid() });
      toast('Order placed: ' + order.id);
      go('/orders/' + order.id);
    } catch (e) { $('#err').textContent = e.message; ev.target.disabled = false; }
  };
}

function authForm(kind) {
  const reg = kind === 'register';
  app.innerHTML = `<form class="card" id="f"><h1>${reg ? 'Create account' : 'Welcome back'}</h1>
    ${reg ? '<label>Name</label><input name="name" required maxlength="100" autocomplete="name">' : ''}
    <label>Email</label><input name="email" type="email" required autocomplete="email">
    <label>Password</label><input name="password" type="password" required minlength="8" maxlength="72" autocomplete="${reg ? 'new-password' : 'current-password'}">
    <p class="err" id="err"></p><button class="btn" style="width:100%">${reg ? 'Register' : 'Login'}</button>
    <p class="muted">${reg ? 'Have an account? <a href="/login" data-link>Login</a>' : 'New here? <a href="/register" data-link>Register</a>'}</p></form>`;
  $('#f').onsubmit = async (e) => {
    e.preventDefault();
    const f = Object.fromEntries(new FormData(e.target));
    try {
      state.user = await api('POST', '/api/auth/' + kind, f);
      await refreshCart();
      const next = new URLSearchParams(location.search).get('next');
      go(next && next.startsWith('/') && !next.startsWith('//') ? next : '/home');
    } catch (err) { $('#err').textContent = err.message; }
  };
}
function loginPage() {
  return authForm('login');
}

function registerPage() {
  return authForm('register');
}

async function cartPage() {
  await refreshCart();
  const c = state.cart;
  if (!c.items.length) { app.innerHTML = '<h1>Your cart</h1><div class="card"><p>Your cart is empty.</p><a class="btn" href="/products" data-link>Browse products</a></div>'; return; }
  app.innerHTML = `<h1>Your cart</h1><div class="split"><div class="card"><table><thead><tr><th>Item</th><th>Price</th><th>Qty</th><th>Total</th><th></th></tr></thead><tbody>
    ${c.items.map((i) => `<tr><td>${esc(i.image)} ${esc(i.name)}</td><td>${money(i.price)}</td>
    <td><span class="qty"><button data-q="${i.product_id}:${i.quantity - 1}">−</button>${i.quantity}<button data-q="${i.product_id}:${Math.min(99, i.quantity + 1)}">+</button></span></td>
    <td>${money(i.line_total)}</td><td><button class="btn danger" data-rm="${i.product_id}">Remove</button></td></tr>`).join('')}
    </tbody></table></div><div class="card"><h2 style="margin-top:0">Summary</h2><div class="row spread"><span>Items</span><b>${c.count}</b></div>
    <div class="row spread"><span>Total</span><b class="price">${money(c.total)}</b></div><br>
    <a class="btn" style="width:100%;text-align:center" href="/checkout" data-link>Checkout</a><br><br>
    <button class="btn ghost" style="width:100%" id="clear">Clear cart</button></div></div>`;
  app.querySelectorAll('[data-q]').forEach((b) => b.onclick = async () => {
    const [id, q] = b.dataset.q.split(':');
    try { state.cart = await api('PUT', `/api/cart/items/${id}`, { quantity: Number(q) }); cartPage(); renderNav(); } catch (e) { toast(e.message); }
  });
  app.querySelectorAll('[data-rm]').forEach((b) => b.onclick = async () => {
    try { state.cart = await api('DELETE', `/api/cart/items/${b.dataset.rm}`); cartPage(); renderNav(); } catch (e) { toast(e.message); }
  });
  $('#clear').onclick = async () => { state.cart = await api('DELETE', '/api/cart'); cartPage(); renderNav(); };
}

async function checkoutPage() {
  const sum = await api('POST', '/api/checkout').catch((e) => { if (e.status === 400) return null; throw e; });
  if (!sum) { app.innerHTML = '<h1>Checkout</h1><div class="card"><p>Your cart is empty.</p></div>'; return; }
  // One idempotency key per checkout attempt: if the request is retried (by the browser, the load balancer
  // or by clicking again) the backend returns the SAME order instead of creating a duplicate.
  if (!state.checkoutKey) state.checkoutKey = uuid();
  app.innerHTML = `<h1>Checkout</h1><div class="split"><form class="card" id="f" style="max-width:none;margin:0">
    <label>Shipping address</label><textarea name="address" required rows="3" maxlength="300" placeholder="221B Baker Street, Mumbai"></textarea>
    <label>Payment</label><input value="Simulated payment (no card needed)" disabled>
    <p class="err" id="err"></p><button class="btn" id="place">Place order</button></form>
    <div class="card"><h2 style="margin-top:0">Order summary</h2>${sum.cart.items.map((i) => `<div class="row spread"><span>${esc(i.name)} × ${i.quantity}</span><span>${money(i.line_total)}</span></div>`).join('')}
    <hr><div class="row spread"><b>Total</b><b class="price">${money(sum.total)}</b></div></div></div>`;
  $('#f').onsubmit = async (e) => {
    e.preventDefault();
    $('#place').disabled = true;
    try {
      const order = await api('POST', '/api/orders', { address: new FormData(e.target).get('address') }, { 'Idempotency-Key': state.checkoutKey });
      state.checkoutKey = null;
      await refreshCart();
      toast('Order placed: ' + order.id);
      go('/orders/' + order.id);
    } catch (err) { $('#err').textContent = err.message + ' — you can safely press Place order again.'; $('#place').disabled = false; }
  };
}

async function ordersPage() {
  const list = await api('GET', '/api/orders');
  app.innerHTML = `<h1>Your orders</h1>` + (list.length ? `<div class="card"><table><thead><tr><th>Order</th><th>Items</th><th>Total</th><th>Status</th><th>Placed</th></tr></thead><tbody>
    ${list.map((o) => `<tr><td><a href="/orders/${esc(o.id)}" data-link>${esc(o.id)}</a></td><td>${o.items.map((i) => `${esc(i.product_name)} × ${i.quantity}`).join(', ')}</td>
    <td>${money(o.total)}</td><td><span class="pill">${esc(o.status)}</span></td><td>${new Date(o.created_at).toLocaleString()}</td></tr>`).join('')}</tbody></table></div>` : '<div class="card"><p>No orders yet.</p></div>');
}

async function orderDetail(id) {
  const o = await api('GET', '/api/orders/' + encodeURIComponent(id));
  app.innerHTML = `<h1>Order ${esc(o.id)}</h1><div class="split"><div class="card"><table><thead><tr><th>Item</th><th>Price</th><th>Qty</th></tr></thead><tbody>
    ${o.items.map((i) => `<tr><td>${esc(i.product_name)}</td><td>${money(i.unit_price)}</td><td>${i.quantity}</td></tr>`).join('')}</tbody></table></div>
    <div class="card"><p><span class="pill">${esc(o.status)}</span> <span class="pill">${esc(o.payment_status)}</span></p><p>Total <b class="price">${money(o.total)}</b></p>
    <p class="muted">Placed ${new Date(o.created_at).toLocaleString()}<br>Ship to: ${esc(o.shipping_address)}<br>Request ID: ${esc(o.request_id)}<br>Handled by: ${esc(o.backend_id)}</p></div></div>`;
}

async function profilePage() {
  const u = await api('GET', '/api/profile');
  app.innerHTML = `<form class="card" id="f"><h1>Profile</h1><label>Name</label><input name="name" value="${esc(u.name)}" required maxlength="100">
    <label>Email</label><input value="${esc(u.email)}" disabled><p class="muted">Member since ${new Date(u.created_at).toLocaleDateString()} · ${u.order_count} orders</p>
    <p class="err" id="err"></p><div class="row"><button class="btn">Save</button><button type="button" class="btn ghost" id="logout">Logout</button></div></form>`;
  $('#f').onsubmit = async (e) => {
    e.preventDefault();
    try { const r = await api('PUT', '/api/profile', { name: new FormData(e.target).get('name') }); state.user.name = r.name; renderNav(); toast('Saved'); } catch (err) { $('#err').textContent = err.message; }
  };
  $('#logout').onclick = async () => { await api('POST', '/api/auth/logout'); state.user = null; state.cart = { count: 0, items: [], total: 0 }; go('/home'); };
}

loadSession().then(route);
