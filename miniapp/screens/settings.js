import { api } from '/miniapp/api/client.js';

const twa = window.Telegram?.WebApp;

const MODE_INFO = {
  disabled:  { label: 'Отключено',        desc: 'AI не обрабатывает сообщения' },
  silent:    { label: 'Наблюдение',        desc: 'Только анализ, без ответов' },
  manual:    { label: 'Ручной режим',      desc: 'Все черновики требуют одобрения' },
  semi_auto: { label: 'Полуавтомат',       desc: 'Уверенные ответы отправляются сами' },
  auto:      { label: 'Полная автономия',  desc: 'AI отвечает самостоятельно' },
};
const MODE_ORDER = ['disabled', 'silent', 'manual', 'semi_auto', 'auto'];

export function renderSettings(navigate, chatId) {
  const el = document.createElement('div');
  el.className = 'screen';
  el.innerHTML = `
    <div class="topbar">
      <button class="btn-icon" id="btn-back">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M15 18l-6-6 6-6"/></svg>
      </button>
      <div class="topbar-title">Настройки чата</div>
    </div>
    <div class="scroll">
      <div class="loading-row"><div class="spinner"></div></div>
    </div>
  `;

  twa?.BackButton?.show();
  twa?.BackButton?.onClick(() => navigate(`/chat/${chatId}`));

  el.querySelector('#btn-back').addEventListener('click', () => {
    twa?.HapticFeedback?.impactOccurred('light');
    navigate(`/chat/${chatId}`);
  });

  let currentSettings = null;

  function renderContent(settings) {
    currentSettings = settings;
    const scroll = el.querySelector('.scroll');
    const available = new Set(settings.available_modes || ['disabled', 'silent', 'manual']);

    const memoryOn = !(settings.privacy_flags || []).includes('no_memory');
    const privateOn = (settings.privacy_flags || []).includes('private');

    scroll.innerHTML = `
      <div class="settings-section">
        <div class="settings-label">Режим автономии</div>
        <div class="mode-list" id="mode-list">
          ${MODE_ORDER.map(mode => {
            const info = MODE_INFO[mode];
            const isAvail = available.has(mode);
            const isActive = settings.mode === mode;
            return `
              <div class="mode-option ${isActive ? 'active' : ''} ${isAvail ? '' : 'locked'}"
                   data-mode="${mode}" style="${isAvail ? '' : 'opacity:0.4;pointer-events:none'}">
                <div class="mode-option-dot"></div>
                <div class="mode-option-body">
                  <div class="mode-option-title">${info.label}${isAvail ? '' : ' 🔒'}</div>
                  <div class="mode-option-desc">${info.desc}</div>
                </div>
              </div>
            `;
          }).join('')}
        </div>
        <div style="font-size:11px;color:var(--hint);padding:6px 4px">
          Расширенные режимы разблокируются по мере одобрения черновиков
        </div>
      </div>

      <div class="settings-section">
        <div class="settings-label">Приватность</div>
        <div class="settings-group">
          <div class="settings-row">
            <div class="settings-row-icon" style="background:rgba(99,102,241,0.14)">🧠</div>
            <div class="settings-row-body">
              <div class="settings-row-title">Память</div>
              <div class="settings-row-subtitle">AI запоминает факты о собеседнике</div>
            </div>
            <label class="toggle">
              <input type="checkbox" id="toggle-memory" ${memoryOn ? 'checked' : ''}>
              <div class="toggle-track"></div>
              <div class="toggle-knob"></div>
            </label>
          </div>
          <div class="settings-row">
            <div class="settings-row-icon" style="background:rgba(239,68,68,0.1)">🔒</div>
            <div class="settings-row-body">
              <div class="settings-row-title">Приватный чат</div>
              <div class="settings-row-subtitle">Блокирует автоответы</div>
            </div>
            <label class="toggle">
              <input type="checkbox" id="toggle-private" ${privateOn ? 'checked' : ''}>
              <div class="toggle-track"></div>
              <div class="toggle-knob"></div>
            </label>
          </div>
        </div>
      </div>

      <div style="padding:16px">
        <button class="btn btn-primary" id="btn-save">Сохранить</button>
      </div>
    `;

    // Mode selection
    scroll.querySelectorAll('.mode-option').forEach(opt => {
      opt.addEventListener('click', () => {
        twa?.HapticFeedback?.selectionChanged();
        scroll.querySelectorAll('.mode-option').forEach(o => o.classList.remove('active'));
        opt.classList.add('active');
      });
    });

    // Save
    scroll.querySelector('#btn-save').addEventListener('click', async () => {
      twa?.HapticFeedback?.impactOccurred('medium');
      const btn = scroll.querySelector('#btn-save');
      btn.disabled = true;
      btn.textContent = 'Сохраняем…';

      const selectedMode = scroll.querySelector('.mode-option.active')?.dataset.mode || settings.mode;
      const noMemory = !scroll.querySelector('#toggle-memory').checked;
      const isPrivate = scroll.querySelector('#toggle-private').checked;
      const flags = [];
      if (noMemory) flags.push('no_memory');
      if (isPrivate) flags.push('private');
      // Preserve other flags that aren't controlled by this UI
      for (const f of (settings.privacy_flags || [])) {
        if (f !== 'no_memory' && f !== 'private') flags.push(f);
      }

      try {
        await api.updateSettings(chatId, { mode: selectedMode, privacy_flags: flags });
        twa?.HapticFeedback?.notificationOccurred('success');
        navigate(`/chat/${chatId}`);
      } catch (err) {
        twa?.HapticFeedback?.notificationOccurred('error');
        twa?.showAlert(`Ошибка: ${err.message}`);
        btn.disabled = false;
        btn.textContent = 'Сохранить';
      }
    });
  }

  // Wire Telegram BackButton cleanup
  const obs = new MutationObserver(() => {
    if (!document.body.contains(el)) { twa?.BackButton?.hide(); obs.disconnect(); }
  });
  obs.observe(document.body, { childList: true, subtree: true });

  api.chatSettings(chatId).then(renderContent).catch(err => {
    el.querySelector('.scroll').innerHTML = `<div class="empty-state"><div class="empty-icon">⚠️</div><div class="empty-subtitle">${err.message}</div></div>`;
  });

  return el;
}
