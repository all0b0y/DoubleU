import { api } from '/miniapp/api/client.js';

const twa = window.Telegram?.WebApp;

function esc(value) {
  return String(value || '').replace(/&/g, '&amp;').replace(/</g, '&lt;')
    .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

/**
 * Owner screen to manage MCP servers (external tool endpoints). The auth header
 * is write-only: it is never returned by the API, only the "заголовок задан"
 * badge (has_auth) is shown. "Проверить" lists the server's tools.
 */
export function renderMCPServers(navigate) {
  const el = document.createElement('div');
  el.className = 'screen';
  el.innerHTML = `
    <div class="chat-topbar">
      <div class="chat-topbar-info">
        <div class="chat-topbar-name">внешние инструменты (MCP)</div>
        <div class="chat-topbar-mode" style="color:var(--hint)">серверы инструментов для ролей</div>
      </div>
    </div>
    <div class="scroll" id="mcp-scroll">
      <div class="loading-row"><div class="spinner"></div></div>
    </div>
    <div style="padding:12px 16px calc(12px + var(--inset-bottom,0px));flex-shrink:0;">
      <button id="btn-mcp-back" class="btn btn-secondary" style="width:100%">Назад</button>
    </div>`;

  const back = () => navigate('/settings');
  twa?.BackButton?.show();
  twa?.BackButton?.onClick(back);
  const observer = new MutationObserver(() => {
    if (!document.body.contains(el)) {
      twa?.BackButton?.offClick(back);
      observer.disconnect();
    }
  });
  observer.observe(document.body, { childList: true, subtree: true });
  el.querySelector('#btn-mcp-back').addEventListener('click', back);

  function serverCard(s) {
    const badge = s.has_auth
      ? `<span class="st-row-desc" style="color:var(--accent)">заголовок задан</span>`
      : '';
    return `
      <div class="st-card" data-server-id="${s.id}" style="margin-bottom:8px;">
        <div class="st-row" style="cursor:default;">
          <div class="st-row-body">
            <p class="st-row-title">${esc(s.name)} ${s.enabled ? '' : '· выключен'}</p>
            <p class="st-row-desc">${esc(s.url)}</p>
            ${badge}
          </div>
        </div>
        <div style="display:flex;gap:8px;padding:8px 12px;">
          <button class="btn btn-secondary" data-check="${s.id}" style="flex:1">Проверить</button>
          <button class="btn btn-secondary" data-del="${s.id}" style="flex:1">Удалить</button>
        </div>
        <div class="st-row-desc" data-tools="${s.id}" style="padding:0 12px 8px;"></div>
      </div>`;
  }

  const addForm = `
    <div class="st-section">
      <p class="st-section-label">добавить сервер</p>
      <div class="st-card" style="padding:12px;display:flex;flex-direction:column;gap:8px;">
        <input id="mcp-name" class="input" placeholder="имя (до 64 символов)" maxlength="64">
        <input id="mcp-url" class="input" placeholder="https://server/mcp">
        <select id="mcp-transport" class="input">
          <option value="http">http (streamable)</option>
          <option value="sse">sse</option>
        </select>
        <input id="mcp-auth" class="input" type="password" placeholder="Authorization header (не обязательно)" autocomplete="off">
        <button id="mcp-add" class="btn btn-primary">Добавить</button>
      </div>
    </div>`;

  function bindCard(root) {
    root.querySelectorAll('[data-del]').forEach(b => b.addEventListener('click', async () => {
      if (!(await confirmAsync('Удалить сервер?'))) return;
      try { await api.deleteMCPServer(b.dataset.del); load(); }
      catch (err) { twa?.showAlert(`Ошибка: ${err.message}`); }
    }));
    root.querySelectorAll('[data-check]').forEach(b => b.addEventListener('click', async () => {
      const out = root.querySelector(`[data-tools="${b.dataset.check}"]`);
      out.textContent = 'проверка…';
      try {
        const res = await api.checkMCPServer(b.dataset.check);
        out.textContent = res.ok
          ? `инструменты: ${(res.tools || []).map(t => t.name).join(', ') || '—'}`
          : `недоступен: ${esc(res.error || 'ошибка')}`;
      } catch (err) { out.textContent = `ошибка: ${esc(err.message)}`; }
    }));
  }

  function confirmAsync(msg) {
    return new Promise(resolve => {
      if (twa?.showConfirm) twa.showConfirm(msg, resolve);
      else resolve(window.confirm(msg));
    });
  }

  async function load() {
    const scroll = el.querySelector('#mcp-scroll');
    try {
      const data = await api.mcpServers();
      const servers = data.servers || [];
      const list = servers.length
        ? servers.map(serverCard).join('')
        : `<p class="st-row-desc" style="padding:8px 4px;">серверов пока нет</p>`;
      scroll.innerHTML = `<div class="st-content">
        <div class="st-section"><p class="st-section-label">серверы</p>${list}</div>
        ${addForm}
      </div>`;
      bindCard(scroll);
      scroll.querySelector('#mcp-add').addEventListener('click', async () => {
        const body = {
          name: scroll.querySelector('#mcp-name').value.trim(),
          url: scroll.querySelector('#mcp-url').value.trim(),
          transport: scroll.querySelector('#mcp-transport').value,
          auth_header: scroll.querySelector('#mcp-auth').value,
        };
        try {
          await api.createMCPServer(body);
          twa?.HapticFeedback?.notificationOccurred('success');
          load();
        } catch (err) {
          twa?.HapticFeedback?.notificationOccurred('error');
          twa?.showAlert(`Ошибка: ${err.message}`);
        }
      });
    } catch (err) {
      scroll.innerHTML = `<div class="empty-state"><div class="empty-icon">⚠️</div>
        <div class="empty-subtitle">${esc(err.message)}</div></div>`;
    }
  }

  load();
  return el;
}
