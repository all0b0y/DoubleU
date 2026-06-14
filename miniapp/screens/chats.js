import { api, openEvents } from '/miniapp/api/client.js';
import { renderBottomNav } from '/miniapp/components/nav.js';

const twa = window.Telegram?.WebApp;

const MODE_DISPLAY = {
  auto:      { icon: '⚡', label: 'auto' },
  semi_auto: { icon: '⚡', label: 'semi_auto' },
  manual:    { icon: '✋', label: 'manual' },
  silent:    { icon: '',  label: 'silent' },
  disabled:  { icon: '',  label: 'disabled' },
};

function initials(name) {
  if (!name) return '?';
  return name.split(/\s+/).map(w => w[0]).join('').slice(0, 2).toUpperCase();
}

function timeAgo(iso) {
  if (!iso) return '';
  const diff = (Date.now() - new Date(iso)) / 1000;
  if (diff < 60)    return 'сейчас';
  if (diff < 3600)  return `${Math.floor(diff / 60)} мин`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} ч`;
  const d = new Date(iso);
  const days = ['вс','пн','вт','ср','чт','пт','сб'];
  if (diff < 7 * 86400) return days[d.getDay()];
  return d.toLocaleDateString('ru', { day: 'numeric', month: 'short' });
}

function modeLabel(mode) {
  const m = MODE_DISPLAY[mode];
  if (!m) return mode;
  return m.icon ? `${m.icon} ${m.label}` : m.label;
}

function chatItemHTML(chat) {
  const hasDrafts = chat.draft_count > 0;
  const avatarClass = hasDrafts ? 'avatar-accent' : 'avatar-muted';
  const mode = chat.mode || 'disabled';
  return `
    <div class="chat-item ${hasDrafts ? 'has-drafts' : ''}" data-id="${chat.chat_id}">
      <div class="avatar avatar-md ${avatarClass}">${initials(chat.display_name)}</div>
      <div class="chat-item-body">
        <div class="chat-item-row1">
          <div class="chat-item-name">${chat.display_name || chat.chat_id}</div>
          ${hasDrafts
            ? `<span class="chat-draft-count">${chat.draft_count}</span>`
            : `<span class="chat-item-time">${timeAgo(chat.last_seen_at)}</span>`
          }
        </div>
        <div class="chat-item-row2">
          <span class="chat-mode-inline ${mode}">${modeLabel(mode)}</span>
          ${chat.ai_role_name ? `<span class="role-chip">🎭 ${chat.ai_role_name}</span>` : ''}
          <span class="chat-item-preview">${chat.last_message || 'Нет сообщений'}</span>
        </div>
      </div>
    </div>
  `;
}

export function renderChats(navigate) {
  const el = document.createElement('div');
  el.className = 'screen';
  el.innerHTML = `
    <div class="chats-header">
      <div class="chats-header-left">
        <span class="chats-header-brand">DoubleU</span>
        <span class="chats-header-title">Чаты</span>
      </div>
      <button class="chats-header-add" id="btn-add" aria-label="Добавить">+</button>
    </div>

    <div class="search-bar" style="margin:0 20px 8px;">
      <span class="search-icon" style="font-size:15px;color:var(--muted)">🔍</span>
      <input type="search" placeholder="поиск" id="search-input" style="font-size:13px;">
    </div>

    <div class="scroll" id="chat-list" style="padding-bottom:0;">
      <div class="loading-row"><div class="spinner"></div></div>
    </div>

    <div id="add-menu-overlay" class="sheet-overlay" style="display:none;">
      <div class="sheet">
        <div class="sheet-handle"></div>
        <div class="sheet-item" data-menu="add-bot">
          <span class="sheet-item-icon">🤖</span>
          <div class="sheet-item-body">
            <p class="sheet-item-title">Подключить бота</p>
            <p class="sheet-item-desc">ИИ будет отвечать в чатах твоего бота</p>
          </div>
        </div>
      </div>
    </div>
  `;
  el.appendChild(renderBottomNav('chats', navigate));

  twa?.BackButton?.hide();

  // Personal chats are disabled in this iteration — the bot section is the only
  // surface. The personal tab and its add-chat entry point were removed from the UI.
  let myChats = [];
  let bot = { connected: false };

  function refreshList() {
    renderList(el.querySelector('#search-input').value);
  }

  // ── Bots section ──────────────────────────────────────────────────────────

  function botSectionHTML(botChats) {
    if (!bot.connected) {
      return `
        <div class="empty-state">
          <div class="empty-icon">🤖</div>
          <div class="empty-title">Бот не подключён</div>
          <div class="empty-subtitle">
            Подключи своего Telegram-бота — ИИ будет отвечать его собеседникам:
            с ролью, памятью и черновиками под твоим контролем.
          </div>
          <button class="btn btn-primary" id="btn-connect-bot" style="max-width:260px;margin-top:8px;">
            Подключить бота
          </button>
        </div>`;
    }
    const statusOk = bot.status === 'active' || !bot.status;
    return `
      <div class="bot-card" id="bot-card">
        <div class="role-card-icon" style="background:rgba(16,185,129,0.16);">🤖</div>
        <div class="chat-item-body">
          <div class="chat-item-row1">
            <div class="role-card-name">@${bot.bot_username || bot.bot_id}</div>
            <span class="role-card-usage" style="${statusOk ? '' : 'color:var(--danger);'}">
              ${statusOk ? `${botChats.length} ${plural(botChats.length, 'чат', 'чата', 'чатов')}` : bot.status}
            </span>
          </div>
          <div class="role-card-desc">ИИ отвечает собеседникам этого бота</div>
        </div>
      </div>
      ${botChats.length
        ? `<div style="display:flex;flex-direction:column;gap:2px;padding:4px 0;">
             ${botChats.map(c => chatItemHTML(c)).join('')}
           </div>`
        : `<div class="empty-state" style="padding:24px;">
             <div class="empty-icon">💬</div>
             <div class="empty-subtitle">Пока никто не написал боту.<br>Как только придёт сообщение — чат появится здесь.</div>
           </div>`}
    `;
  }

  function plural(n, one, few, many) {
    const m10 = n % 10, m100 = n % 100;
    if (m10 === 1 && m100 !== 11) return one;
    if (m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14)) return few;
    return many;
  }

  // ── List rendering ────────────────────────────────────────────────────────

  function renderList(filter = '') {
    const list = el.querySelector('#chat-list');
    const q = filter.toLowerCase();
    const match = c => !q || (c.display_name || '').toLowerCase().includes(q);

    const botChats = myChats.filter(c => c.transport === 'bot' && c.claimed).filter(match);
    list.innerHTML = botSectionHTML(botChats);

    list.querySelector('#btn-connect-bot')?.addEventListener('click', () => {
      twa?.HapticFeedback?.impactOccurred('medium');
      navigate('/add-bot');
    });

    list.querySelectorAll('.chat-item').forEach(item => {
      item.addEventListener('click', () => {
        twa?.HapticFeedback?.selectionChanged();
        navigate(`/chat/${item.dataset.id}`);
      });
    });
  }

  async function loadChats() {
    try {
      const [chatsData, botData] = await Promise.all([
        api.chats(),
        api.getBot().catch(() => ({ connected: false })),
      ]);
      myChats = chatsData.chats || [];
      bot = botData || { connected: false };
      refreshList();
    } catch (err) {
      el.querySelector('#chat-list').innerHTML = `
        <div class="empty-state">
          <div class="empty-icon">⚠️</div>
          <div class="empty-title">Ошибка</div>
          <div class="empty-subtitle">${err.message}</div>
        </div>`;
    }
  }

  // ── "+" burger menu ───────────────────────────────────────────────────────

  const overlay = el.querySelector('#add-menu-overlay');
  el.querySelector('#btn-add').addEventListener('click', () => {
    twa?.HapticFeedback?.impactOccurred('light');
    overlay.style.display = 'flex';
  });
  overlay.addEventListener('click', (e) => {
    if (e.target === overlay) overlay.style.display = 'none';
  });
  overlay.querySelector('[data-menu="add-bot"]').addEventListener('click', () => {
    twa?.HapticFeedback?.selectionChanged();
    overlay.style.display = 'none';
    navigate('/add-bot');
  });

  el.querySelector('#search-input').addEventListener('input', e => renderList(e.target.value));

  // Real-time
  const es = openEvents({
    new_message: () => loadChats(),
    new_draft: () => loadChats(),
    chat_mode_changed: () => loadChats(),
    chat_role_changed: () => loadChats(),
    chats_updated: () => loadChats(),
  });
  const obs = new MutationObserver(() => {
    if (!document.body.contains(el)) { es.close(); obs.disconnect(); }
  });
  obs.observe(document.body, { childList: true, subtree: true });

  loadChats();
  return el;
}
