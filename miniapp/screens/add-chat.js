import { api } from '/miniapp/api/client.js';

const twa = window.Telegram?.WebApp;

function initials(name) {
  if (!name) return '?';
  return name.split(/\s+/).map(w => w[0]).join('').slice(0, 2).toUpperCase();
}

export function renderAddChat(navigate) {
  const el = document.createElement('div');
  el.className = 'screen';
  el.innerHTML = `
    <div class="topbar">
      <button class="btn-icon" id="btn-back">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M15 18l-6-6 6-6"/></svg>
      </button>
      <div class="topbar-title">Добавить чаты</div>
      <button class="topbar-action" id="btn-add" style="color:var(--accent)">Добавить</button>
    </div>
    <div class="search-bar">
      <span class="search-icon">🔍</span>
      <input type="search" placeholder="Поиск" id="search-input">
    </div>
    <div id="selected-bar" style="display:none;padding:8px 16px;font-size:13px;color:var(--hint)">
      Выбрано: <span id="selected-count">0</span>
    </div>
    <div class="scroll" id="contact-list">
      <div class="loading-row"><div class="spinner"></div></div>
    </div>
  `;

  twa?.BackButton?.show();
  twa?.BackButton?.onClick(() => navigate('/chats'));

  const selected = new Set();
  let allContacts = [];

  function updateAddButton() {
    const bar = el.querySelector('#selected-bar');
    const countEl = el.querySelector('#selected-count');
    const addBtn = el.querySelector('#btn-add');
    bar.style.display = selected.size ? '' : 'none';
    countEl.textContent = selected.size;
    addBtn.style.display = selected.size ? '' : 'none';
  }

  function renderContacts(filter = '') {
    const list = el.querySelector('#contact-list');
    const filtered = filter
      ? allContacts.filter(c => c.display_name.toLowerCase().includes(filter.toLowerCase()))
      : allContacts;

    if (!filtered.length) {
      list.innerHTML = `
        <div class="empty-state">
          <div class="empty-icon">👥</div>
          <div class="empty-title">Нет доступных чатов</div>
          <div class="empty-subtitle">Сначала получи сообщение через Telegram Business — тогда контакт появится здесь</div>
        </div>`;
      return;
    }

    list.innerHTML = filtered.map(c => `
      <div class="contact-item" data-id="${c.chat_id}">
        <div class="contact-check ${selected.has(c.chat_id) ? 'checked' : ''}" data-id="${c.chat_id}"></div>
        <div class="avatar avatar-sm">${initials(c.display_name)}</div>
        <div style="flex:1;min-width:0">
          <div style="font-size:14px;font-weight:500;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${c.display_name || c.chat_id}</div>
        </div>
      </div>
    `).join('');

    list.querySelectorAll('.contact-item').forEach(item => {
      item.addEventListener('click', () => {
        twa?.HapticFeedback?.selectionChanged();
        const id = item.dataset.id;
        if (selected.has(id)) selected.delete(id);
        else selected.add(id);
        const check = item.querySelector('.contact-check');
        check.classList.toggle('checked', selected.has(id));
        updateAddButton();
      });
    });
  }

  async function loadContacts() {
    try {
      const data = await api.contacts();
      allContacts = data.contacts || [];
      renderContacts();
    } catch (err) {
      el.querySelector('#contact-list').innerHTML = `
        <div class="empty-state"><div class="empty-icon">⚠️</div><div class="empty-subtitle">${err.message}</div></div>`;
    }
  }

  el.querySelector('#btn-back').addEventListener('click', () => {
    twa?.HapticFeedback?.impactOccurred('light');
    navigate('/chats');
  });

  el.querySelector('#search-input').addEventListener('input', (e) => renderContacts(e.target.value));

  el.querySelector('#btn-add').addEventListener('click', async () => {
    if (!selected.size) return;
    twa?.HapticFeedback?.impactOccurred('medium');
    const btn = el.querySelector('#btn-add');
    btn.disabled = true;
    btn.textContent = '…';
    try {
      await api.addChats([...selected]);
      twa?.HapticFeedback?.notificationOccurred('success');
      navigate('/chats');
    } catch (err) {
      twa?.HapticFeedback?.notificationOccurred('error');
      twa?.showAlert(`Ошибка: ${err.message}`);
      btn.disabled = false;
      btn.textContent = 'Добавить';
    }
  });

  // BackButton cleanup
  const obs = new MutationObserver(() => {
    if (!document.body.contains(el)) { twa?.BackButton?.hide(); obs.disconnect(); }
  });
  obs.observe(document.body, { childList: true, subtree: true });

  updateAddButton();
  loadContacts();
  return el;
}
