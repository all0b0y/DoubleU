import { api, openEvents } from '/miniapp/api/client.js';
import { renderWelcome }   from '/miniapp/screens/welcome.js';
import { renderOnboarding } from '/miniapp/screens/onboarding.js';
import { renderChats }     from '/miniapp/screens/chats.js';
import { renderChat }      from '/miniapp/screens/chat.js';
import { renderSettings }  from '/miniapp/screens/settings.js';
import { renderAddChat }   from '/miniapp/screens/add-chat.js';

const twa = window.Telegram?.WebApp;
const app = document.getElementById('app');

// ── TMA initialisation ────────────────────────────────────────────────────────
twa?.ready();
twa?.expand();

// Apply Telegram theme to document
if (twa?.colorScheme === 'light') document.documentElement.classList.add('light');
twa?.onEvent('themeChanged', () => {
  document.documentElement.classList.toggle('light', twa.colorScheme === 'light');
});

// ── Global event bus ─────────────────────────────────────────────────────────
const bus = new EventTarget();
export function emit(type, detail) { bus.dispatchEvent(new CustomEvent(type, { detail })); }
export function on(type, fn)       { bus.addEventListener(type, (e) => fn(e.detail)); }

// ── Router ───────────────────────────────────────────────────────────────────
const routes = {
  '/':                    () => renderWelcome(navigate),
  '/onboarding':          (_, q) => renderOnboarding(navigate, q?.reason),
  '/chats':               () => renderChats(navigate),
  '/chat/:id':            (p) => renderChat(navigate, p.id),
  '/chat/:id/settings':   (p) => renderSettings(navigate, p.id),
  '/add-chat':            () => renderAddChat(navigate),
};

export function navigate(path) {
  window.location.hash = '#' + path;
}

function matchRoute(hash) {
  const [pathPart, queryPart] = hash.replace(/^#/, '').split('?');
  const segments = pathPart.split('/').filter(Boolean);
  const query = Object.fromEntries(new URLSearchParams(queryPart));

  for (const [pattern, handler] of Object.entries(routes)) {
    const patSegs = pattern.split('/').filter(Boolean);
    if (patSegs.length !== segments.length) continue;
    const params = {};
    let match = true;
    for (let i = 0; i < patSegs.length; i++) {
      if (patSegs[i].startsWith(':')) {
        params[patSegs[i].slice(1)] = decodeURIComponent(segments[i]);
      } else if (patSegs[i] !== segments[i]) {
        match = false; break;
      }
    }
    if (match) return { handler, params, query };
  }
  return null;
}

async function route() {
  const hash = window.location.hash || '#/';
  const found = matchRoute(hash);
  if (!found) { navigate('/chats'); return; }

  // Guard: check connection status before any protected route
  const protected_ = ['/chats', '/chat', '/add-chat', '/chat/:id/settings'];
  const needsAuth = protected_.some(p => hash.replace('#', '').startsWith(p.replace(':id', '')));
  if (needsAuth) {
    try {
      const me = await api.me();
      if (me.connection.status === 'revoked') { navigate('/onboarding?reason=revoked'); return; }
      if (me.connection.status === 'not_found') { navigate('/onboarding'); return; }
    } catch (_) {
      // If API is down, show welcome so the user knows the app is loading.
      renderWelcome(navigate);
      return;
    }
  }

  const screen = found.handler(found.params, found.query);
  if (screen) {
    app.innerHTML = '';
    app.appendChild(screen);
  }
}

window.addEventListener('hashchange', route);

// ── Real-time SSE ─────────────────────────────────────────────────────────────
let evtSource = null;

function connectSSE() {
  evtSource?.close();
  evtSource = openEvents({
    new_draft: (data) => {
      emit('new_draft', data);
      // Refresh chats list if visible
      if (window.location.hash === '#/chats') route();
    },
    draft_status: (data) => emit('draft_status', data),
    new_message:  (data) => emit('new_message', data),
    chat_mode_changed: (data) => emit('chat_mode_changed', data),
  });
}

// ── Boot ─────────────────────────────────────────────────────────────────────
async function boot() {
  if (!window.location.hash || window.location.hash === '#/') {
    // Check auth before deciding where to land
    try {
      const me = await api.me();
      if (me.connection.status === 'connected') {
        navigate('/chats');
      } else if (me.connection.status === 'revoked') {
        navigate('/onboarding?reason=revoked');
      } else {
        navigate('/onboarding');
      }
    } catch (_) {
      navigate('/');
    }
  } else {
    await route();
  }
  connectSSE();
}

boot();
