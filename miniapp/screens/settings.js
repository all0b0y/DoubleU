import { api } from '/miniapp/api/client.js';

const twa = window.Telegram?.WebApp;

const MODE_INFO = {
  disabled:  { label: 'disabled',  desc: 'DoubleU не работает в этом чате' },
  silent:    { label: 'silent',    desc: 'только анализ, без черновиков' },
  manual:    { label: 'manual',    desc: 'черновики только по запросу' },
  semi_auto: { label: 'semi_auto', desc: 'генерит, ты подтверждаешь' },
  auto:      { label: 'auto',      desc: 'отвечает самостоятельно' },
};
const MODE_ORDER = ['disabled', 'silent', 'manual', 'semi_auto', 'auto'];

export function renderSettings(navigate, chatId) {
  const el = document.createElement('div');
  el.className = 'screen';
  el.innerHTML = `
    <div class="chat-topbar">
      <div class="chat-topbar-info">
        <div class="chat-topbar-name">настройки чата</div>
        <div class="chat-topbar-mode" id="st-chatname" style="color:var(--hint)">…</div>
      </div>
    </div>
    <div class="scroll" id="st-scroll">
      <div class="loading-row"><div class="spinner"></div></div>
    </div>
  `;

  const handleBack = () => navigate(`/chat/${chatId}`);
  twa?.BackButton?.show();
  twa?.BackButton?.onClick(handleBack);

  let currentMode = 'manual';
  let currentFlags = [];
  let availableModes = new Set(['disabled', 'silent', 'manual']);
  let contextScore = 0;

  function modeRowHTML(mode) {
    const info = MODE_INFO[mode] || { label: mode, desc: '' };
    const isActive  = currentMode === mode;
    const isLocked  = !availableModes.has(mode);

    if (isLocked) {
      const threshold = mode === 'auto' ? 80 : 50;
      return `
        <div class="st-mode-row st-mode-locked">
          <span class="st-mode-icon">🔒</span>
          <div class="st-mode-body">
            <p class="st-mode-name st-mode-name-locked">${info.label}</p>
            <p class="st-mode-desc">${info.desc}</p>
          </div>
          <span class="st-mode-lock-badge">${contextScore} / ${threshold}</span>
        </div>`;
    }
    return `
      <div class="st-mode-row ${isActive ? 'st-mode-active' : ''}" data-mode="${mode}">
        <span class="st-mode-icon">${isActive ? '◉' : '○'}</span>
        <div class="st-mode-body">
          <p class="st-mode-name ${isActive ? 'st-mode-name-active' : ''}">${info.label}</p>
          <p class="st-mode-desc">${info.desc}</p>
        </div>
      </div>`;
  }

  function renderContent(settings) {
    currentMode  = settings.mode || 'manual';
    currentFlags = settings.privacy_flags || [];
    availableModes = new Set(settings.available_modes || ['disabled', 'silent', 'manual']);
    contextScore = settings.context_score ?? 0;

    const memoryOn  = !currentFlags.includes('no_memory');

    el.querySelector('#st-scroll').innerHTML = `
      <div class="st-content">

        <!-- режим автоматизации -->
        <div class="st-section">
          <p class="st-section-label">режим автоматизации</p>
          <div class="st-card" id="mode-list">
            ${MODE_ORDER.map((m, i) => `
              <div class="st-card-divider-wrap ${i < MODE_ORDER.length - 1 ? 'st-divider' : ''}">
                ${modeRowHTML(m)}
              </div>`).join('')}
          </div>
        </div>

        <!-- память -->
        <div class="st-section">
          <p class="st-section-label">память</p>
          <div class="st-card">
            <div class="st-row">
              <span class="st-row-icon">🧠</span>
              <div class="st-row-body">
                <p class="st-row-title">помнить контекст</p>
                <p class="st-row-desc">учитывать прошлые сообщения</p>
              </div>
              <label class="toggle">
                <input type="checkbox" id="toggle-memory" ${memoryOn ? 'checked' : ''}>
                <div class="toggle-track"></div>
                <div class="toggle-knob"></div>
              </label>
            </div>
          </div>
        </div>

        <!-- дополнительно -->
        <div class="st-section">
          <p class="st-section-label">дополнительно</p>
          <div class="st-card">
            <div class="st-card-divider-wrap st-divider">
              <div class="st-row st-row-link" id="btn-profile-link">
                <span class="st-row-icon">ⓘ</span>
                <div class="st-row-body">
                  <p class="st-row-title">досье собеседника</p>
                </div>
                <span class="st-chevron">›</span>
              </div>
            </div>
            <div class="st-card-divider-wrap">
              <div class="st-row st-row-danger" id="btn-delete">
                <span class="st-row-icon">🗑</span>
                <div class="st-row-body">
                  <p class="st-row-title" style="color:var(--danger)">удалить чат из DoubleU</p>
                </div>
              </div>
            </div>
          </div>
        </div>

      </div>
    `;

    wireInteractions();
  }

  function wireInteractions() {
    // Mode selection — immediate save
    el.querySelectorAll('[data-mode]').forEach(row => {
      row.addEventListener('click', async () => {
        const mode = row.dataset.mode;
        if (mode === currentMode) return;
        twa?.HapticFeedback?.selectionChanged();
        currentMode = mode;
        // Re-render mode list only
        el.querySelectorAll('#mode-list .st-card-divider-wrap').forEach((wrap, i) => {
          const m = MODE_ORDER[i];
          wrap.querySelector('.st-mode-row').outerHTML;
          wrap.innerHTML = modeRowHTML(m);
        });
        wireInteractions();
        try {
          await api.updateSettings(chatId, { mode, privacy_flags: currentFlags });
        } catch (err) {
          twa?.showAlert(`Ошибка: ${err.message}`);
        }
      });
    });

    // Memory toggle — immediate save
    const memToggle = el.querySelector('#toggle-memory');
    if (memToggle) {
      memToggle.addEventListener('change', async () => {
        twa?.HapticFeedback?.selectionChanged();
        const noMemory = !memToggle.checked;
        currentFlags = noMemory
          ? [...new Set([...currentFlags, 'no_memory'])]
          : currentFlags.filter(f => f !== 'no_memory');
        try {
          await api.updateSettings(chatId, { mode: currentMode, privacy_flags: currentFlags });
        } catch (err) {
          twa?.showAlert(`Ошибка: ${err.message}`);
        }
      });
    }

    // Досье
    el.querySelector('#btn-profile-link')?.addEventListener('click', () => {
      twa?.HapticFeedback?.impactOccurred('light');
      navigate(`/chat/${chatId}/profile`);
    });

    // Удалить
    el.querySelector('#btn-delete')?.addEventListener('click', () => {
      twa?.HapticFeedback?.impactOccurred('medium');
      twa?.showPopup({
        title: 'Удалить чат?',
        message: 'DoubleU перестанет обрабатывать этот чат. Историю восстановить нельзя.',
        buttons: [
          { id: 'cancel', type: 'cancel', text: 'Отмена' },
          { id: 'delete', type: 'destructive', text: 'Удалить' },
        ],
      }, (btnId) => {
        if (btnId === 'delete') {
          twa?.HapticFeedback?.notificationOccurred('warning');
          twa?.showAlert('Функция будет доступна в следующем обновлении.');
        }
      });
    });
  }

  const obs = new MutationObserver(() => {
    if (!document.body.contains(el)) {
      twa?.BackButton?.offClick(handleBack);
      obs.disconnect();
    }
  });
  obs.observe(document.body, { childList: true, subtree: true });

  // Load chat name + settings
  Promise.all([api.chats(), api.chatSettings(chatId)]).then(([chatsData, settings]) => {
    const chat = chatsData.chats?.find(c => c.chat_id === chatId);
    if (chat) el.querySelector('#st-chatname').textContent = chat.display_name || chatId;
    renderContent(settings);
  }).catch(err => {
    el.querySelector('#st-scroll').innerHTML = `
      <div class="empty-state"><div class="empty-icon">⚠️</div><div class="empty-subtitle">${err.message}</div></div>`;
  });

  return el;
}
