import { api, openEvents } from '/miniapp/api/client.js';

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
  const now = new Date();
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
          <span class="chat-item-preview">${chat.last_message || 'Нет сообщений'}</span>
        </div>
      </div>
    </div>
  `;
}

function suggestedItemHTML(chat) {
  return `
    <div class="chat-item" data-id="${chat.chat_id}" data-suggested="true">
      <div class="avatar avatar-md avatar-muted">${initials(chat.display_name)}</div>
      <div class="chat-item-body">
        <div class="chat-item-row1">
          <div class="chat-item-name">${chat.display_name || chat.chat_id}</div>
          <button class="suggested-add-btn" data-add="${chat.chat_id}">+ Добавить</button>
        </div>
        <div class="chat-item-row2">
          <span class="chat-item-preview" style="color:var(--muted)">новый контакт</span>
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
      <button class="chats-header-add" id="btn-add" aria-label="Добавить чат">+</button>
    </div>

    <div class="search-bar" style="margin:0 20px 8px;">
      <span class="search-icon" style="font-size:15px;color:var(--muted)">🔍</span>
      <input type="search" placeholder="поиск" id="search-input" style="font-size:13px;">
    </div>

    <div class="scroll" id="chat-list" style="padding-bottom:0;">
      <div class="loading-row"><div class="spinner"></div></div>
    </div>

    <nav class="bottom-nav">
      <button class="bottom-nav-item active">
        <span class="bottom-nav-icon">💬</span>
        <span class="bottom-nav-label">чаты</span>
      </button>
      <button class="bottom-nav-item">
        <span class="bottom-nav-icon">🎭</span>
        <span class="bottom-nav-label">роли</span>
      </button>
      <button class="bottom-nav-item">
        <span class="bottom-nav-icon">🛍</span>
        <span class="bottom-nav-label">маркет</span>
      </button>
      <button class="bottom-nav-item">
        <span class="bottom-nav-icon">👤</span>
        <span class="bottom-nav-label">я</span>
      </button>
    </nav>
  `;

  // Hide Telegram back button on root screen
  twa?.BackButton?.hide();

  let myChats = [];
  let suggestedChats = [];

  function renderList(filter = '') {
    const list = el.querySelector('#chat-list');
    const q = filter.toLowerCase();

    const mine = q ? myChats.filter(c => (c.display_name || '').toLowerCase().includes(q)) : myChats;
    const sug  = q ? suggestedChats.filter(c => (c.display_name || '').toLowerCase().includes(q)) : suggestedChats;

    let html = '';

    if (mine.length) {
      html += `<div style="display:flex;flex-direction:column;gap:2px;padding:4px 0;">
        ${mine.map(c => chatItemHTML(c)).join('')}
      </div>`;
    }

    if (sug.length) {
      html += `
        <div class="section-header" style="margin-top:8px;">
          <span class="section-title">Предложения</span>
          <span style="font-size:11px;color:var(--muted);">новые контакты</span>
        </div>
        <div style="display:flex;flex-direction:column;gap:2px;padding:4px 0;">
          ${sug.map(c => suggestedItemHTML(c)).join('')}
        </div>`;
    }

    if (!mine.length && !sug.length) {
      html = `
        <div class="empty-state">
          <div class="empty-icon">💬</div>
          <div class="empty-title">Нет чатов</div>
          <div class="empty-subtitle">Нажми + чтобы добавить или получи первое бизнес-сообщение в Telegram</div>
        </div>`;
    }

    list.innerHTML = html;

    // Navigate to chat
    list.querySelectorAll('.chat-item').forEach(item => {
      item.addEventListener('click', (e) => {
        if (e.target.closest('[data-add]')) return;
        if (item.dataset.suggested === 'true') return;
        twa?.HapticFeedback?.selectionChanged();
        navigate(`/chat/${item.dataset.id}`);
      });
    });

    // "+ Добавить" in suggested
    list.querySelectorAll('[data-add]').forEach(btn => {
      btn.addEventListener('click', async (e) => {
        e.stopPropagation();
        twa?.HapticFeedback?.impactOccurred('medium');
        btn.textContent = '…'; btn.disabled = true;
        try {
          await api.addChats([btn.dataset.add]);
          twa?.HapticFeedback?.notificationOccurred('success');
          await loadChats();
        } catch {
          btn.textContent = '+ Добавить'; btn.disabled = false;
        }
      });
    });
  }

  async function loadChats() {
    try {
      const [chatsData, contactsData] = await Promise.all([api.chats(), api.contacts()]);
      myChats = chatsData.chats || [];
      suggestedChats = contactsData.contacts || [];
      renderList(el.querySelector('#search-input').value);
    } catch (err) {
      el.querySelector('#chat-list').innerHTML = `
        <div class="empty-state">
          <div class="empty-icon">⚠️</div>
          <div class="empty-title">Ошибка</div>
          <div class="empty-subtitle">${err.message}</div>
        </div>`;
    }
  }

  el.querySelector('#btn-add').addEventListener('click', () => {
    twa?.HapticFeedback?.impactOccurred('light');
    navigate('/add-chat');
  });

  el.querySelector('#search-input').addEventListener('input', e => renderList(e.target.value));

  // Real-time
  const es = openEvents({
    new_draft: () => loadChats(),
    chat_mode_changed: () => loadChats(),
  });
  const obs = new MutationObserver(() => {
    if (!document.body.contains(el)) { es.close(); obs.disconnect(); }
  });
  obs.observe(document.body, { childList: true, subtree: true });

  loadChats();
  return el;
}
