import { api, openEvents } from '/miniapp/api/client.js';
import { renderWelcome }   from '/miniapp/screens/welcome.js';
import { renderChats }     from '/miniapp/screens/chats.js';
import { renderChat }      from '/miniapp/screens/chat.js';
import { renderSettings }  from '/miniapp/screens/settings.js';
import { renderAddChat }   from '/miniapp/screens/add-chat.js';
import { renderProfile }   from '/miniapp/screens/profile.js';
import { renderRoles }     from '/miniapp/screens/roles.js';
import { renderRoleEdit }  from '/miniapp/screens/role-edit.js';
import { renderRoleTools } from '/miniapp/screens/role-tools.js';
import { renderMe }        from '/miniapp/screens/me.js';
import { renderMCPServers } from '/miniapp/screens/mcp-servers.js';
import { renderSkills }     from '/miniapp/screens/skills.js';
import { renderAddBot, renderBotSettings } from '/miniapp/screens/add-bot.js';
import { brandify }        from '/miniapp/components/glyphs.js';

const twa = window.Telegram?.WebApp;
const app = document.getElementById('app');

// Swap system emoji for brand glyphs across every screen, including content
// rendered asynchronously (chat lists, messages, drafts). One central pass keeps
// the templates readable and guarantees nothing slips through. brandify mutates
// the DOM, so we detach the observer while it runs to avoid re-triggering.
const glyphObserver = new MutationObserver(() => {
  glyphObserver.disconnect();
  brandify(app);
  glyphObserver.observe(app, { childList: true, subtree: true, characterData: true });
});
glyphObserver.observe(app, { childList: true, subtree: true, characterData: true });

// ── TMA initialisation ────────────────────────────────────────────────────────
twa?.ready();
twa?.expand();
// requestFullscreen throws WebAppMethodUnsupported (not just warns) on TWA < 8.0,
// which would abort this whole module and leave a black screen.
try { twa?.requestFullscreen?.(); } catch (_) { /* older Telegram client */ }
try {
  // Cream brand surface — match --bg in styles/main.css.
  twa?.setHeaderColor?.('#F7F1DF');
  twa?.setBackgroundColor?.('#F7F1DF');
  twa?.setBottomBarColor?.('#F7F1DF');
} catch (_) { /* older Telegram client */ }

// Push our UI below Telegram's native header in fullscreen mode.
// safeAreaInset.top = status bar; contentSafeAreaInset.top = Telegram header.
function applyInsets() {
  const safeTop    = twa?.safeAreaInset?.top        ?? 0;
  const contentTop = twa?.contentSafeAreaInset?.top ?? 0;
  document.documentElement.style.setProperty('--inset-top', `${safeTop + contentTop}px`);
  const safeBottom    = twa?.safeAreaInset?.bottom        ?? 0;
  const contentBottom = twa?.contentSafeAreaInset?.bottom ?? 0;
  document.documentElement.style.setProperty('--inset-bottom', `${safeBottom + contentBottom}px`);
}
applyInsets();
twa?.onEvent?.('safeAreaChanged', applyInsets);
twa?.onEvent?.('contentSafeAreaChanged', applyInsets);

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
  '/chats':               () => renderChats(navigate),
  '/chat/:id':            (p) => renderChat(navigate, p.id),
  '/chat/:id/settings':   (p) => renderSettings(navigate, p.id),
  '/chat/:id/profile':    (p) => renderProfile(navigate, p.id),
  '/add-chat':            () => renderAddChat(navigate),
  '/add-bot':             () => renderAddBot(navigate),
  '/bot/:id':             (p) => renderBotSettings(navigate, p.id),
  // '/roles/new' must precede '/roles/:id' — the router matches in order.
  '/roles':               () => renderRoles(navigate),
  '/roles/new':           () => renderRoleEdit(navigate, null),
  '/roles/:id/tools':     (p) => renderRoleTools(navigate, p.id),
  '/roles/:id':           (p) => renderRoleEdit(navigate, p.id),
  '/mcp-servers':         () => renderMCPServers(navigate),
  '/skills':              () => renderSkills(navigate),
  '/me':                  () => renderMe(navigate),
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

  // Guard: if the API is unreachable, fall back to the splash so the user
  // isn't left on a blank screen. Connection status no longer gates routing —
  // the Telegram-setup onboarding screen has been removed.
  const protected_ = ['/chats', '/chat', '/add-chat', '/add-bot', '/chat/:id/settings', '/roles', '/me'];
  const needsAuth = protected_.some(p => hash.replace('#', '').startsWith(p.replace(':id', '')));
  if (needsAuth) {
    try {
      await api.me();
    } catch (_) {
      navigate('/'); return;
    }
  }

  const screen = found.handler(found.params, found.query);
  if (screen) {
    app.innerHTML = '';
    app.appendChild(screen);
    brandify(screen); // swap emoji before first paint (observer covers async updates)
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

// ── Timezone auto-detection ──────────────────────────────────────────────────
// The scheduler prompt (ai-python _current_time_instruction) needs the owner's
// IANA timezone to compute fire_at correctly; without it reminders fall back
// to UTC. Runs once per detected zone (cached in localStorage) and never blocks
// boot — it's a best-effort background sync, not part of the critical path.
async function syncTimezone() {
  try {
    const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
    if (!tz) return;
    if (localStorage.getItem('tz_synced') === tz) return;
    const s = await api.userSettings();
    await api.saveUserSettings({ ...s, timezone: tz });
    localStorage.setItem('tz_synced', tz);
  } catch (_) {
    // Background sync — never surface a failure to the user.
  }
}

// ── Boot ─────────────────────────────────────────────────────────────────────
async function boot() {
  if (!window.location.hash || window.location.hash === '#/') {
    // Land straight in the app; fall back to the splash if the API is down.
    try {
      await api.me();
      syncTimezone(); // fire-and-forget; must not delay navigation
      navigate('/chats');
    } catch (_) {
      navigate('/');
    }
  } else {
    await route();
  }
  connectSSE();
}

boot();
