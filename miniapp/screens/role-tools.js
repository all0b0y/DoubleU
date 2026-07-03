import { api } from '/miniapp/api/client.js';

const twa = window.Telegram?.WebApp;

const CATEGORY_META = {
  read:        { label: 'память и чтение', icon: '◉' },
  schedule:    { label: 'расписание', icon: '◷' },
  integration: { label: 'интеграции', icon: '↗' },
  interaction: { label: 'взаимодействие', icon: '◇' },
  skills:      { label: 'навыки', icon: '✦' },
};

function esc(value) {
  return String(value || '').replace(/&/g, '&amp;').replace(/</g, '&lt;')
    .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

/** Render the separate per-role native-tool settings screen. */
export function renderRoleTools(navigate, roleId) {
  const el = document.createElement('div');
  el.className = 'screen';
  el.innerHTML = `
    <div class="chat-topbar">
      <div class="chat-topbar-info">
        <div class="chat-topbar-name">инструменты</div>
        <div class="chat-topbar-mode" id="rt-subtitle" style="color:var(--hint)">…</div>
      </div>
    </div>
    <div class="scroll" id="rt-scroll">
      <div class="loading-row"><div class="spinner"></div></div>
    </div>
    <div style="padding:12px 16px calc(12px + var(--inset-bottom,0px));flex-shrink:0;display:flex;gap:8px;">
      <button id="btn-tools-back" class="btn btn-secondary">Назад</button>
      <button id="btn-tools-save" class="btn btn-primary" disabled>Сохранить</button>
    </div>`;

  const back = () => navigate(`/roles/${roleId}`);
  twa?.BackButton?.show();
  twa?.BackButton?.onClick(back);
  const observer = new MutationObserver(() => {
    if (!document.body.contains(el)) {
      twa?.BackButton?.offClick(back);
      observer.disconnect();
    }
  });
  observer.observe(document.body, { childList: true, subtree: true });

  let toolSettings = {};

  function effectiveEnabled(tool) {
    return Object.prototype.hasOwnProperty.call(toolSettings, tool.key)
      ? toolSettings[tool.key]
      : Boolean(tool.default_enabled);
  }

  function renderCatalog(tools) {
    const grouped = Object.groupBy
      ? Object.groupBy(tools, tool => tool.category)
      : tools.reduce((groups, tool) => {
          (groups[tool.category] ||= []).push(tool);
          return groups;
        }, {});
    const sections = Object.keys(CATEGORY_META).map(category => {
      const categoryTools = grouped[category] || [];
      if (!categoryTools.length) return '';
      const meta = CATEGORY_META[category];
      return `
        <div class="st-section">
          <p class="st-section-label">${meta.label}</p>
          <div class="st-card">
            ${categoryTools.map((tool, index) => `
              <div class="${index < categoryTools.length - 1 ? 'st-divider' : ''}">
                <div class="st-row" style="cursor:default;">
                  <span class="st-row-icon">${meta.icon}</span>
                  <div class="st-row-body">
                    <p class="st-row-title">${esc(tool.label)}</p>
                    <p class="st-row-desc">${esc(tool.description)}</p>
                  </div>
                  <label class="toggle">
                    <input type="checkbox" data-tool-key="${esc(tool.key)}" ${effectiveEnabled(tool) ? 'checked' : ''}>
                    <div class="toggle-track"></div>
                    <div class="toggle-knob"></div>
                  </label>
                </div>
              </div>`).join('')}
          </div>
        </div>`;
    }).join('');
    el.querySelector('#rt-scroll').innerHTML = `<div class="st-content">${sections}</div>`;
    el.querySelectorAll('[data-tool-key]').forEach(input => {
      input.addEventListener('change', () => {
        toolSettings[input.dataset.toolKey] = input.checked;
      });
    });
  }

  el.querySelector('#btn-tools-back').addEventListener('click', back);
  el.querySelector('#btn-tools-save').addEventListener('click', async () => {
    const button = el.querySelector('#btn-tools-save');
    button.textContent = '…';
    button.disabled = true;
    try {
      await api.saveRoleTools(roleId, toolSettings);
      twa?.HapticFeedback?.notificationOccurred('success');
      back();
    } catch (err) {
      twa?.HapticFeedback?.notificationOccurred('error');
      twa?.showAlert(`Ошибка: ${err.message}`);
      button.textContent = 'Сохранить';
      button.disabled = false;
    }
  });

  Promise.all([api.toolCatalog(), api.roles()]).then(([catalog, rolesData]) => {
    const role = (rolesData.roles || []).find(item => String(item.id) === String(roleId));
    if (!role) {
      navigate('/roles');
      return;
    }
    toolSettings = { ...(role.tool_settings || {}) };
    el.querySelector('#rt-subtitle').textContent = role.name;
    renderCatalog(catalog.tools || []);
    el.querySelector('#btn-tools-save').disabled = false;
  }).catch(err => {
    el.querySelector('#rt-scroll').innerHTML = `
      <div class="empty-state"><div class="empty-icon">⚠️</div><div class="empty-subtitle">${esc(err.message)}</div></div>`;
  });

  return el;
}
