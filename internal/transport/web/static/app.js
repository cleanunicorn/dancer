/* dancer web UI — a thin client over the web transport's API.
   Everything shown comes from the server: the channel and thread lists
   and a thread's past from /api/state and /api/threads, what happens now
   from /api/events. The page keeps only what it is looking at. */
(() => {
'use strict';

const $ = (id) => document.getElementById(id);
const store = {
  get: (k, d) => { try { const v = localStorage.getItem('dancer.' + k); return v == null ? d : JSON.parse(v); } catch { return d; } },
  set: (k, v) => localStorage.setItem('dancer.' + k, JSON.stringify(v)),
};
const ME = 'web'; // Inbound.Transport of this UI

const state = {
  me: store.get('name', ''),
  channels: new Map(),        // id -> {id, name, transport}
  threads: new Map(),         // id -> thread (+ unread, live, waiting)
  messages: new Map(),        // thread id -> [message], only for threads opened this session
  current: null,              // thread id being viewed
  draft: null,                // {channel} while composing a new thread; {channel, text} once sent
  seen: store.get('seen', {}),// thread id -> last viewed "updated"
  connected: false,
};

/* ---------- API ---------- */

async function api(method, path, body) {
  const res = await fetch(path, {
    method, headers: body ? { 'Content-Type': 'application/json' } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) { await login(); return api(method, path, body); }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

let loginPending = null;
function login() {
  if (loginPending) return loginPending;
  loginPending = new Promise((resolve) => {
    const dlg = $('dlg-login'), input = $('token-input'), err = $('login-error');
    err.hidden = true; input.value = '';
    dlg.showModal(); input.focus();
    dlg.onclose = async () => {
      const res = await fetch('/api/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ token: input.value }) });
      if (res.ok) { loginPending = null; resolve(); return; }
      const data = await res.json().catch(() => ({}));
      err.textContent = data.error || 'wrong token'; err.hidden = false;
      dlg.showModal(); input.select();
    };
  });
  return loginPending;
}

/* ---------- bootstrap ---------- */

async function main() {
  await api('GET', '/api/state').catch(() => {}); // asks for the token first if one is needed
  if (!state.me) await askName();
  $('app').hidden = false;
  $('me').textContent = state.me;
  await loadState();
  connect();
  const hash = decodeURIComponent(location.hash.slice(1));
  if (hash.startsWith('new:')) newThread(hash.slice(4));
  else if (hash && state.threads.has(hash)) openThread(hash);
}

async function loadState() {
  const st = await api('GET', '/api/state');
  const old = state.threads;
  state.channels.clear(); state.threads = new Map();
  for (const c of st.channels) state.channels.set(c.id, c);
  for (const t of st.threads) {
    const prev = old.get(t.id);
    state.threads.set(t.id, prev ? Object.assign(prev, t) : t);
  }
  renderSidebar();
}

function askName() {
  return new Promise((resolve) => {
    const dlg = $('dlg-name'), input = $('name-input');
    input.value = state.me;
    dlg.showModal(); input.focus();
    dlg.onclose = () => {
      const v = input.value.trim();
      if (!v) { dlg.showModal(); return; }
      state.me = v; store.set('name', v); $('me').textContent = v;
      if ('Notification' in window && Notification.permission === 'default') Notification.requestPermission();
      resolve();
    };
  });
}

/* ---------- events ---------- */

let es = null;
function connect() {
  if (es) es.close();
  es = new EventSource('/api/events');
  es.onopen = () => setConnected(true);
  es.onerror = () => setConnected(false);
  es.onmessage = (e) => {
    let ev; try { ev = JSON.parse(e.data); } catch { return; }
    handleEvent(ev);
  };
}

let everConnected = false;
function setConnected(on) {
  if (state.connected === on) return;
  state.connected = on;
  $('conn').classList.toggle('off', !on);
  $('conn').title = on ? 'Connected' : 'Reconnecting…';
  if (on && everConnected) loadState().then(() => { if (state.current) reloadThread(state.current); });
  if (on) everConnected = true;
}

function handleEvent(ev) {
  switch (ev.type) {
    case 'thread': {
      const t = ev.threadInfo;
      state.threads.set(t.id, Object.assign(state.threads.get(t.id) || {}, t));
      renderSidebar();
      if (state.current === t.id) renderHead();
      if (state.draft && state.draft.text && state.draft.channel === t.channel) openThread(t.id);
      return;
    }
    case 'message': addMessage(ev.message); return;
    case 'edit': editMessage(ev.message); return;
    case 'remove': removeMessage(ev.thread, ev.id); return;
  }
}

function thread(id, channel) {
  // a thread the sidebar has not listed yet (no task yet): make a stub
  let t = state.threads.get(id);
  if (!t) {
    t = { id, channel: channel || id.split('/')[0], transport: '', title: '', updated: new Date().toISOString() };
    state.threads.set(id, t);
  }
  return t;
}

function addMessage(m) {
  const list = state.messages.get(m.thread);
  if (list) {
    if (list.some((x) => x.id === m.id)) return;
    list.push(m);
  }
  const t = thread(m.thread);
  t.updated = m.at;
  if (!t.title && m.from && m.text) t.title = firstLine(m.text);
  if (m.prompt) t.waiting = true;
  else if (m.decision || !m.from) t.waiting = false;
  const mine = m.from && m.from.via === ME && m.from.name === state.me;
  if (mine && state.draft && state.draft.text && t.channel === state.draft.channel) {
    openThread(m.thread);
  } else if ((state.current !== m.thread || document.hidden) && !mine) {
    t.unread = (t.unread || 0) + 1;
  }
  if (state.current === m.thread) { renderMessages(); markSeen(t); }
  renderSidebar();
  notify(m, t);
}

function editMessage(m) {
  const list = state.messages.get(m.thread);
  if (list) {
    const cur = list.find((x) => x.id === m.id);
    if (cur) Object.assign(cur, m); else list.push(m);
  }
  thread(m.thread).live = m.text;
  if (state.current === m.thread) renderLive();
  renderSidebar();
}

function removeMessage(th, id) {
  const list = state.messages.get(th);
  if (list) {
    const i = list.findIndex((x) => x.id === id);
    if (i >= 0) list.splice(i, 1);
  }
  const t = state.threads.get(th);
  if (t) t.live = '';
  if (state.current === th) renderLive();
  renderSidebar();
}

function notify(m, t) {
  if (!('Notification' in window) || Notification.permission !== 'granted') return;
  if (!m.prompt) return;
  if (m.mention && m.mention !== state.me) return;
  if (!document.hidden && state.current === m.thread) return;
  const n = new Notification('dancer needs an answer', { body: (t.title ? t.title + ' — ' : '') + strip(m.text).slice(0, 120), tag: m.thread });
  n.onclick = () => { window.focus(); openThread(m.thread); };
}

/* ---------- sidebar ---------- */

function renderSidebar() {
  const nav = $('channels');
  const groups = new Map(); // transport -> channels
  for (const c of state.channels.values()) {
    if (!groups.has(c.transport)) groups.set(c.transport, []);
    groups.get(c.transport).push(c);
  }
  const byChannel = new Map();
  for (const t of state.threads.values()) {
    const key = t.channel;
    if (!byChannel.has(key)) byChannel.set(key, []);
    byChannel.get(key).push(t);
  }
  // threads whose channel no transport lists (a DM, an old channel)
  for (const [ch, threads] of byChannel) {
    if (state.channels.has(ch)) continue;
    const tr = threads[0].transport || 'other';
    if (!groups.has(tr)) groups.set(tr, []);
    groups.get(tr).push({ id: ch, name: ch, transport: tr, implicit: true });
  }
  const frag = document.createDocumentFragment();
  const order = [...groups.keys()].sort((a, b) => (a === ME ? -1 : b === ME ? 1 : a.localeCompare(b)));
  if (!groups.size) frag.appendChild(el('div', 'none', 'No channels — check server.transports'));
  for (const tr of order) {
    const g = el('div', 'group');
    g.appendChild(el('div', 'group-title', label(tr)));
    const chans = groups.get(tr).sort((a, b) => a.name.localeCompare(b.name));
    for (const c of chans) {
      const box = el('div', 'channel');
      const row = el('div', 'channel-row');
      row.appendChild(el('span', 'hash', '#'));
      row.appendChild(el('span', '', c.name));
      if (!c.implicit) {
        const b = el('button', 'icon-btn small new', '＋'); b.title = 'New thread in #' + c.name;
        b.onclick = () => newThread(c.id);
        row.appendChild(b);
      }
      box.appendChild(row);
      const threads = (byChannel.get(c.id) || []).sort((a, b) => new Date(b.updated) - new Date(a.updated));
      if (!threads.length) box.appendChild(el('div', 'none', 'no threads yet — press ＋'));
      for (const t of threads.slice(0, 50)) box.appendChild(threadRow(t));
      if (threads.length > 50) box.appendChild(el('div', 'none', (threads.length - 50) + ' older threads not shown'));
      g.appendChild(box);
    }
    frag.appendChild(g);
  }
  nav.replaceChildren(frag);
  updateTitle();
}

function label(transport) {
  if (transport === ME) return 'Web';
  return transport.charAt(0).toUpperCase() + transport.slice(1);
}

function threadRow(t) {
  const fresh = t.unread || (t.updated && state.seen[t.id] !== t.updated && state.current !== t.id && t.requester === state.me);
  const a = el('a', 'thread' + (t.id === state.current ? ' active' : '') + (fresh ? ' unread' : '') + (t.closed ? ' closed' : ''));
  a.href = '#' + t.id;
  a.onclick = (e) => { e.preventDefault(); openThread(t.id); };
  const icon = stateIcon(t);
  const st = el('span', 't-state', icon.text); st.title = icon.title;
  a.appendChild(st);
  a.appendChild(el('span', 't-title', t.title || t.id));
  if (t.unread) a.appendChild(el('span', 't-badge', String(t.unread)));
  else a.appendChild(el('span', 't-time', ago(t.updated)));
  return a;
}

function stateIcon(t) {
  if (t.waiting) return { text: '✋', title: 'waiting for an answer' };
  if (t.live) return { text: '⏳', title: t.live };
  if (t.status === 'running' || t.status === 'queued') return { text: '⏳', title: t.status };
  if (t.status === 'failed') return { text: '❌', title: 'failed' };
  if (t.closed) return { text: '✓', title: 'closed' };
  return { text: '', title: t.status || '' };
}

function markSeen(t) {
  t.unread = 0;
  state.seen[t.id] = t.updated;
  store.set('seen', state.seen);
}

function updateTitle() {
  let waiting = 0, unreadN = 0;
  for (const t of state.threads.values()) { if (t.waiting) waiting++; unreadN += t.unread || 0; }
  document.title = (waiting ? '✋ ' : '') + (unreadN ? '(' + unreadN + ') ' : '') + 'dancer';
}

/* ---------- thread view ---------- */

async function openThread(id) {
  state.current = id; state.draft = null;
  location.hash = id;
  $('sidebar').classList.remove('open');
  const t = thread(id);
  markSeen(t);
  renderSidebar(); renderHead();
  $('composer').hidden = false;
  $('input').placeholder = 'Reply… (Enter to send, Shift+Enter for a new line)';
  stickBottom = true;
  if (!state.messages.has(id)) await reloadThread(id);
  else renderMessages(true);
  $('input').focus();
}

async function reloadThread(id) {
  try {
    const data = await api('GET', '/api/threads/' + id);
    state.messages.set(id, data.messages || []);
    const t = thread(id);
    const live = (data.messages || []).find((m) => m.key);
    t.live = live ? live.text : '';
    const last = lastNonLive(data.messages || []);
    if (last) t.waiting = !!(last.prompt && !answerFor(data.messages, last.prompt.id));
  } catch (e) { toast(e.message); return; }
  if (state.current === id) renderMessages(true);
  renderSidebar();
}

function newThread(channel) {
  const c = state.channels.get(channel);
  if (!c) return;
  state.current = null; state.draft = { channel };
  location.hash = 'new:' + channel;
  $('sidebar').classList.remove('open');
  renderSidebar();
  $('head-title').textContent = 'New thread in #' + c.name;
  const sub = $('head-sub');
  sub.replaceChildren(el('span', 'badge', label(c.transport)), document.createTextNode(' Describe the task, or "run <agent> <prompt>" to pick the agent'));
  const empty = el('div', 'empty');
  empty.append(el('div', 'empty-art', '✨'), el('p', '', c.transport === ME
    ? 'Your first message starts the thread — and the task.'
    : 'Your first message is posted in ' + label(c.transport) + ' as the start of a new thread, and the task runs there.'));
  $('messages').replaceChildren(empty);
  $('composer').hidden = false;
  $('input').placeholder = 'What should the agent do?';
  $('input').focus();
}

function renderHead() {
  const t = state.threads.get(state.current);
  if (!t) return;
  const c = state.channels.get(t.channel);
  $('head-title').textContent = t.title || t.id;
  const sub = $('head-sub');
  sub.replaceChildren(document.createTextNode('#' + (c ? c.name : t.channel)));
  if (t.transport && t.transport !== ME) sub.appendChild(el('span', 'badge', label(t.transport)));
  if (t.status) sub.appendChild(el('span', 'badge', t.closed ? 'closed' : t.status));
  if (t.requester) sub.appendChild(document.createTextNode(' · started by ' + t.requester));
}

function lastNonLive(list) {
  for (let i = list.length - 1; i >= 0; i--) if (!list[i].key) return list[i];
  return null;
}

// answerFor finds the decision message that settled prompt id.
function answerFor(list, promptId) {
  return list.find((m) => m.decision && m.decision.promptId === promptId) || null;
}

let stickBottom = true;
function renderMessages(force) {
  const box = $('messages');
  const list = state.messages.get(state.current) || [];
  const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 80;
  const frag = document.createDocumentFragment();
  const last = lastNonLive(list);
  for (const m of list) {
    if (m.key) continue;
    if (m.decision && list.some((p) => p.prompt && p.prompt.id === m.decision.promptId)) continue; // shown on the prompt
    frag.appendChild(messageEl(m, list, m === last));
  }
  const live = list.find((m) => m.key);
  if (live) frag.appendChild(liveEl(live));
  box.replaceChildren(frag);
  if (force || atBottom || stickBottom) box.scrollTop = box.scrollHeight;
  else showNewBelow();
}

function renderLive() {
  const box = $('messages');
  const list = state.messages.get(state.current) || [];
  const live = list.find((m) => m.key);
  const cur = box.querySelector('.msg.live');
  const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 80;
  if (cur) cur.remove();
  if (live) box.appendChild(liveEl(live));
  if (atBottom) box.scrollTop = box.scrollHeight;
}

function showNewBelow() {
  const box = $('messages');
  if (box.querySelector('.new-below')) return;
  const b = el('button', 'new-below', 'New messages ↓');
  b.onclick = () => { box.scrollTop = box.scrollHeight; b.remove(); };
  box.appendChild(b);
}

function liveEl(m) {
  const d = el('div', 'msg live');
  d.appendChild(el('div', 'avatar', ''));
  const body = el('div', 'body');
  const bubble = el('div', 'bubble');
  bubble.appendChild(el('span', 'spinner', ''));
  bubble.appendChild(el('span', '', m.text));
  body.appendChild(bubble); d.appendChild(body);
  return d;
}

function messageEl(m, list, isLast) {
  const kind = m.from ? 'human' : m.markdown ? 'agent' : 'system';
  const d = el('div', 'msg ' + kind + (kind === 'system' ? ' ' + systemClass(m.text) : ''));
  d.dataset.id = m.id;
  if (m.mention && m.mention === state.me) d.classList.add('for-me');
  if (m.from && m.from.via === ME && m.from.name === state.me) d.classList.add('mine');
  d.appendChild(el('div', 'avatar', kind === 'human' ? initials(m.from.name) : kind === 'agent' ? '🤖' : ''));
  const body = el('div', 'body');
  if (kind !== 'system') {
    const meta = el('div', 'meta');
    meta.appendChild(el('b', '', kind === 'human' ? m.from.name : 'agent'));
    if (m.from && m.from.via !== ME) meta.appendChild(el('span', 'via', 'via ' + label(m.from.via)));
    if (m.at && !m.at.startsWith('0001')) meta.appendChild(el('span', '', time(m.at)));
    body.appendChild(meta);
  }
  const bubble = el('div', 'bubble');
  if (m.decision) bubble.innerHTML = '→ <b>' + esc(m.decision.choice) + '</b>';
  else if (kind === 'agent') { bubble.classList.add('md'); bubble.innerHTML = md(m.text); }
  else if (kind === 'human') bubble.innerHTML = plain(m.text);
  else bubble.innerHTML = mrkdwn(m.text, m.mention);
  if (kind === 'system' && m.at && !m.at.startsWith('0001')) bubble.title = time(m.at);
  if (m.files && m.files.length) bubble.appendChild(filesEl(m.files));
  if (m.prompt) bubble.appendChild(promptEl(m, answerFor(list, m.prompt.id), isLast));
  body.appendChild(bubble); d.appendChild(body);
  return d;
}

function systemClass(text) {
  if (text.startsWith('❌')) return 'error';
  if (text.startsWith('✅')) return 'done';
  if (/^(⏸️|⚠️|🚫|⏹️|♻️)/.test(text)) return 'notice';
  return '';
}

function filesEl(files) {
  const box = el('div', 'files');
  for (const f of files) {
    if (!f.data) { box.appendChild(el('span', 'file-gone', '📎 ' + f.name + ' (' + size(f.size) + ', too large to show)')); continue; }
    const url = URL.createObjectURL(new Blob([Uint8Array.from(atob(f.data), (c) => c.charCodeAt(0))]));
    if (/\.(png|jpe?g|gif|webp|svg)$/i.test(f.name)) {
      const a = el('a'); a.href = url; a.target = '_blank';
      const img = el('img'); img.src = url; img.alt = f.name; a.appendChild(img);
      box.appendChild(a);
    } else {
      const a = el('a', '', '📎 ' + f.name + ' (' + size(f.size) + ')'); a.href = url; a.download = f.name;
      box.appendChild(a);
    }
  }
  return box;
}

function promptEl(m, answer, isLast) {
  const p = m.prompt, box = el('div', 'prompt');
  if (answer) {
    const a = el('div', 'answered');
    a.append('✓ ', el('b', '', answer.decision.choice));
    if (answer.from) a.append(' — ' + answer.from.name + (answer.from.via !== ME ? ' via ' + label(answer.from.via) : ''));
    box.appendChild(a);
    return box;
  }
  if (!isLast) { box.appendChild(el('div', 'stale', 'settled')); return box; }
  const choices = el('div', 'choices');
  const send = (choice) => decide(m, choice, choices);
  if (p.options && p.options.length) {
    for (const o of p.options) {
      const b = el('button', '', o.label || o.value);
      if (o.description) b.appendChild(el('small', '', o.description));
      b.onclick = () => send(o.value);
      choices.appendChild(b);
    }
  } else {
    for (const c of p.choices || []) {
      const b = el('button', c === 'allow' ? 'allow' : c === 'deny' ? 'deny' : '', c);
      b.onclick = () => send(c);
      choices.appendChild(b);
    }
  }
  box.appendChild(choices);
  if (p.freeText) {
    const f = el('div', 'free');
    const input = el('input'); input.placeholder = 'or type your own answer…';
    const b = el('button', '', 'Answer');
    const go = () => { if (input.value.trim()) send(input.value.trim()); };
    input.onkeydown = (e) => { if (e.key === 'Enter') { e.preventDefault(); go(); } };
    b.onclick = go;
    f.append(input, b); box.appendChild(f);
  }
  return box;
}

async function decide(m, choice, choices) {
  for (const b of choices.querySelectorAll('button')) b.disabled = true;
  try {
    await api('POST', '/api/decide', { thread: m.thread, promptId: m.prompt.id, choice, user: state.me });
    // the relayed decision arrives on the event stream and settles the prompt
  } catch (e) {
    toast(e.message);
    for (const b of choices.querySelectorAll('button')) b.disabled = false;
  }
}

/* ---------- composer ---------- */

const input = $('input');
$('composer').onsubmit = (e) => { e.preventDefault(); send(); };
input.onkeydown = (e) => {
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); }
};
input.oninput = () => { input.style.height = 'auto'; input.style.height = Math.min(input.scrollHeight, 200) + 'px'; };

async function send() {
  const text = input.value.trim();
  if (!text) return;
  const body = { text, user: state.me };
  if (state.current) body.thread = state.current;
  else if (state.draft) { body.channel = state.draft.channel; state.draft.text = text; }
  else return;
  $('send').disabled = true;
  try {
    await api('POST', '/api/messages', body);
    input.value = ''; input.style.height = 'auto';
    stickBottom = true;
    if (state.draft) {
      $('messages').replaceChildren(el('div', 'empty', 'Opening the thread…'));
      setTimeout(() => { if (state.draft && state.draft.text === text) { state.draft.text = null; toast('The thread did not open — see the server log'); newThread(state.draft.channel); } }, 15000);
    }
  } catch (e) { toast(e.message); if (state.draft) state.draft.text = null; }
  $('send').disabled = false;
  input.focus();
}

$('messages').onscroll = () => {
  const box = $('messages');
  stickBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 80;
  if (stickBottom) { const b = box.querySelector('.new-below'); if (b) b.remove(); }
};

/* ---------- chrome ---------- */

$('btn-menu').onclick = () => $('sidebar').classList.toggle('open');
$('btn-help').onclick = () => $('dlg-help').showModal();
$('me').onclick = () => askName();
document.addEventListener('keydown', (e) => {
  if (e.key === '/' && document.activeElement !== input && !document.querySelector('dialog[open]')) { e.preventDefault(); input.focus(); }
  if (e.key === 'Escape') $('sidebar').classList.remove('open');
});
window.addEventListener('hashchange', () => {
  const h = decodeURIComponent(location.hash.slice(1));
  if (h.startsWith('new:')) { if (!state.draft || state.draft.channel !== h.slice(4)) newThread(h.slice(4)); }
  else if (h && h !== state.current && state.threads.has(h)) openThread(h);
});
document.addEventListener('visibilitychange', () => {
  if (!document.hidden && state.current) {
    const t = state.threads.get(state.current);
    if (t) { markSeen(t); renderSidebar(); }
  }
});
setInterval(renderSidebar, 60000); // relative times

let toastTimer = null;
function toast(text) {
  let t = document.querySelector('.toast');
  if (!t) { t = el('div', 'toast'); document.body.appendChild(t); }
  t.textContent = text;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.remove(), 4000);
}

/* ---------- helpers ---------- */

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
}
function esc(s) { return s.replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])); }
function initials(name) { return (name || '?').split(/\s+/).map((w) => w[0]).join('').slice(0, 2).toUpperCase(); }
function size(n) { return n < 1024 ? n + ' B' : n < 1048576 ? (n / 1024).toFixed(1) + ' KB' : (n / 1048576).toFixed(1) + ' MB'; }
function time(at) { const d = new Date(at); return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }); }
function ago(at) {
  const s = Math.max(0, (Date.now() - new Date(at)) / 1000);
  if (s < 60) return 'now';
  if (s < 3600) return Math.floor(s / 60) + 'm';
  if (s < 86400) return Math.floor(s / 3600) + 'h';
  return Math.floor(s / 86400) + 'd';
}
function firstLine(s) { s = s.trim(); const i = s.indexOf('\n'); if (i >= 0) s = s.slice(0, i); return s.length > 120 ? s.slice(0, 119) + '…' : s; }
function strip(s) { return s.replace(/[*_`]/g, ''); }
function safeUrl(u) { return /^(https?:|mailto:)/i.test(u) || /^[^:]*$/.test(u) ? u : '#'; }
function linkify(s) {
  return s.replace(/(^|[\s(])((https?:\/\/)[^\s<)]+)/g, (m, pre, url) => pre + '<a href="' + url + '" target="_blank" rel="noopener">' + url + '</a>');
}

// plain: escaped text with line breaks and links.
function plain(s) { return linkify(esc(s)).replace(/\n/g, '<br>'); }

// mrkdwn renders dancer's own lines: *bold*, _italic_, `code`, ```fences```, @mentions.
function mrkdwn(s, mention) {
  const parts = s.split(/```/);
  let out = '';
  for (let i = 0; i < parts.length; i++) {
    if (i % 2) { out += '<pre><code>' + esc(parts[i].replace(/^\n/, '')) + '</code></pre>'; continue; }
    let t = esc(parts[i]);
    t = t.replace(/`([^`\n]+)`/g, '<code>$1</code>');
    t = t.replace(/(^|[\s(])\*([^*\n]+)\*(?=[\s).,:;!?]|$)/g, '$1<b>$2</b>');
    t = t.replace(/(^|[\s(])_([^_\n]+)_(?=[\s).,:;!?]|$)/g, '$1<i>$2</i>');
    t = linkify(t).replace(/\n/g, '<br>');
    out += t;
  }
  if (mention) out = '<span class="mention">@' + esc(mention) + '</span> ' + out;
  return out;
}

/* ---------- markdown (CommonMark-ish, enough for an agent's prose) ---------- */

function inline(s) {
  const codes = [];
  s = esc(s);
  s = s.replace(/`([^`]+)`/g, (m, c) => { codes.push(c); return ' ' + (codes.length - 1) + ' '; });
  s = s.replace(/!\[([^\]]*)\]\(([^)\s]+)\)/g, (m, alt, url) => '<img alt="' + alt + '" src="' + safeUrl(url) + '">');
  s = s.replace(/\[([^\]]+)\]\(([^)\s]+)(?:\s+&quot;[^&]*&quot;)?\)/g, (m, txt, url) => '<a href="' + safeUrl(url) + '" target="_blank" rel="noopener">' + txt + '</a>');
  s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
  s = s.replace(/__([^_]+)__/g, '<strong>$1</strong>');
  s = s.replace(/(^|[^*\w])\*([^*\n]+)\*(?!\w)/g, '$1<em>$2</em>');
  s = s.replace(/(^|[^_\w])_([^_\n]+)_(?!\w)/g, '$1<em>$2</em>');
  s = s.replace(/~~([^~]+)~~/g, '<del>$1</del>');
  s = s.replace(/(^|[\s(])(https?:\/\/[^\s<)]+)/g, '$1<a href="$2" target="_blank" rel="noopener">$2</a>');
  s = s.replace(/ (\d+) /g, (m, i) => '<code>' + codes[+i] + '</code>');
  return s;
}

function md(src) {
  const lines = src.replace(/\r\n?/g, '\n').split('\n');
  let out = '', i = 0;
  const para = [];
  const flush = () => { if (para.length) { out += '<p>' + para.map(inline).join('<br>') + '</p>'; para.length = 0; } };
  while (i < lines.length) {
    const line = lines[i];
    let m;
    if ((m = /^\s*(`{3,}|~{3,})\s*(\w*)/.exec(line))) {
      flush();
      const fence = m[1][0]; const lang = m[2];
      const buf = []; i++;
      const close = new RegExp('^\\s*\\' + fence + '{3,}\\s*$');
      while (i < lines.length && !close.test(lines[i])) buf.push(lines[i++]);
      i++;
      out += '<pre><code' + (lang ? ' class="lang-' + esc(lang) + '"' : '') + '>' + esc(buf.join('\n')) + '</code></pre>';
      continue;
    }
    if ((m = /^(#{1,6})\s+(.*?)\s*#*\s*$/.exec(line))) { flush(); out += '<h' + m[1].length + '>' + inline(m[2]) + '</h' + m[1].length + '>'; i++; continue; }
    if (/^\s*([-*_])(\s*\1){2,}\s*$/.test(line)) { flush(); out += '<hr>'; i++; continue; }
    if (/^\s*>/.test(line)) {
      flush(); const buf = [];
      while (i < lines.length && /^\s*>/.test(lines[i])) buf.push(lines[i++].replace(/^\s*>\s?/, ''));
      out += '<blockquote>' + md(buf.join('\n')) + '</blockquote>'; continue;
    }
    if (/^\s*\|/.test(line) && i + 1 < lines.length && /^\s*\|?\s*:?-{2,}/.test(lines[i + 1])) {
      flush();
      const cells = (l) => l.trim().replace(/^\||\|$/g, '').split('|').map((c) => c.trim());
      const head = cells(line); i += 2;
      let t = '<table><thead><tr>' + head.map((c) => '<th>' + inline(c) + '</th>').join('') + '</tr></thead><tbody>';
      while (i < lines.length && /^\s*\|/.test(lines[i])) t += '<tr>' + cells(lines[i++]).map((c) => '<td>' + inline(c) + '</td>').join('') + '</tr>';
      out += t + '</tbody></table>'; continue;
    }
    if (/^\s*([-*+]|\d+[.)])\s+/.test(line)) {
      flush();
      const items = []; // {indent, ordered, lines}
      while (i < lines.length) {
        const l = lines[i];
        const im = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/.exec(l);
        if (im) { items.push({ indent: im[1].length, ordered: /\d/.test(im[2]), lines: [im[3]] }); i++; continue; }
        if (items.length && /^\s+\S/.test(l)) { items[items.length - 1].lines.push(l.trim()); i++; continue; }
        break;
      }
      out += list(items, 0).html;
      continue;
    }
    if (/^\s*$/.test(line)) { flush(); i++; continue; }
    para.push(line); i++;
  }
  flush();
  return out;
}

// list renders items[from…] at their indent level; deeper items become a
// sub-list inside the item before them.
function list(items, from) {
  const base = items[from].indent, ordered = items[from].ordered;
  let html = ordered ? '<ol>' : '<ul>';
  let i = from;
  while (i < items.length && items[i].indent >= base) {
    if (items[i].indent > base && i > from) {
      const r = list(items, i);
      html = html.slice(0, -5) + r.html + '</li>';
      i = r.next; continue;
    }
    const it = items[i];
    const task = /^\[([ xX])\]\s+(.*)$/.exec(it.lines[0]);
    if (task) {
      html += '<li class="task"><input type="checkbox" disabled' + (task[1] !== ' ' ? ' checked' : '') + '> ' + [task[2], ...it.lines.slice(1)].map(inline).join('<br>') + '</li>';
    } else {
      html += '<li>' + it.lines.map(inline).join('<br>') + '</li>';
    }
    i++;
  }
  html += ordered ? '</ol>' : '</ul>';
  return { html, next: i };
}

main().catch((e) => { console.error(e); toast(e.message); });
})();
