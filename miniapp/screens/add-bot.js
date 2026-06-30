import { api } from '/miniapp/api/client.js';

const twa = window.Telegram?.WebApp;

function esc(str) {
  return String(str || '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

function chooseBotRoleContextPolicy(roleName) {
  return new Promise((resolve) => {
    const message = `Выбери память для "${roleName}".`;
    if (twa?.showPopup) {
      twa.showPopup({
        title: 'Память роли',
        message,
        buttons: [
          { id: 'reuse_role_context', type: 'default', text: 'Использовать память' },
          { id: 'new_role_context', type: 'destructive', text: 'Начать чистую' },
          { id: 'cancel', type: 'cancel', text: 'Отмена' },
        ],
      }, (btnId) => {
        resolve(btnId === 'reuse_role_context' || btnId === 'new_role_context' ? btnId : null);
      });
      return;
    }

    const choice = window.prompt(
      `${message}\n\nВведите reuse_role_context или new_role_context.`,
      '',
    )?.trim();
    resolve(choice === 'reuse_role_context' || choice === 'new_role_context' ? choice : null);
  });
}

function nullableBoolMode(value) {
  if (value === true) return 'on';
  if (value === false) return 'off';
  return 'inherit';
}

function modeToNullableBool(mode) {
  if (mode === 'on') return true;
  if (mode === 'off') return false;
  return null;
}

function deliverySelectHTML(id, label, hint, value) {
  const mode = nullableBoolMode(value);
  return `
    <div class="field-group">
      <div class="field-label">${label}</div>
      <select class="field-select" id="${id}">
        <option value="inherit" ${mode === 'inherit' ? 'selected' : ''}>наследовать</option>
        <option value="on" ${mode === 'on' ? 'selected' : ''}>включено</option>
        <option value="off" ${mode === 'off' ? 'selected' : ''}>выключено</option>
      </select>
      <div style="font-size:11px;color:var(--muted);line-height:1.4;margin-top:6px;">${hint}</div>
    </div>`;
}

/**
 * Connect a new Telegram bot. A tenant may connect several, so this is always
 * the token-entry form (managing an existing bot lives in renderBotSettings).
 */
export function renderAddBot(navigate) {
  const el = document.createElement('div');
  el.className = 'screen';
  el.innerHTML = `
    <div class="chat-topbar">
      <div class="chat-topbar-info">
        <div class="chat-topbar-name">подключение бота</div>
        <div class="chat-topbar-mode" style="color:var(--hint)">ИИ будет отвечать его собеседникам</div>
      </div>
    </div>
    <div class="scroll" id="ab-scroll">
      <div class="onboarding-steps">
        <div class="step-card">
          <div class="step-num">1</div>
          <div>
            <div class="step-body-title">Создай бота в @BotFather</div>
            <div class="step-body-text">Открой @BotFather в Telegram, отправь /newbot и придумай имя. Если бот уже есть — используй /token.</div>
          </div>
        </div>
        <div class="step-card">
          <div class="step-num">2</div>
          <div>
            <div class="step-body-title">Скопируй токен</div>
            <div class="step-body-text">BotFather пришлёт токен вида <code>123456:ABC-DEF…</code> — скопируй его целиком.</div>
          </div>
        </div>
        <div class="step-card">
          <div class="step-num">3</div>
          <div>
            <div class="step-body-title">Вставь токен сюда</div>
            <div class="step-body-text">Токен хранится на сервере и никогда не показывается в приложении.</div>
          </div>
        </div>

        <div class="field-group" style="margin-top:4px;">
          <div class="field-label">токен бота</div>
          <input class="field-input" id="bot-token" placeholder="123456:ABC-DEF…"
            autocomplete="off" autocorrect="off" autocapitalize="off" spellcheck="false">
          <div id="bot-error" style="font-size:12px;color:var(--danger);display:none;"></div>
        </div>
        <button class="btn btn-primary" id="btn-connect">Подключить</button>
      </div>
    </div>
  `;

  const handleBack = () => navigate('/chats');
  twa?.BackButton?.show();
  twa?.BackButton?.onClick(handleBack);
  const obs = new MutationObserver(() => {
    if (!document.body.contains(el)) {
      twa?.BackButton?.offClick(handleBack);
      obs.disconnect();
    }
  });
  obs.observe(document.body, { childList: true, subtree: true });

  const btn = el.querySelector('#btn-connect');
  btn.addEventListener('click', async () => {
    const token = el.querySelector('#bot-token').value.trim();
    const errBox = el.querySelector('#bot-error');
    errBox.style.display = 'none';
    if (!token) {
      twa?.HapticFeedback?.notificationOccurred('error');
      errBox.textContent = 'Вставь токен из @BotFather.';
      errBox.style.display = 'block';
      return;
    }
    twa?.HapticFeedback?.impactOccurred('medium');
    btn.textContent = 'Проверяем токен…'; btn.disabled = true;
    try {
      const bot = await api.registerBot(token);
      twa?.HapticFeedback?.notificationOccurred('success');
      twa?.showPopup?.({
        title: 'Бот подключён',
        message: `@${bot.bot_username || bot.bot_id} теперь под управлением DoubleU. Напиши ему — чат появится на главном экране.`,
        buttons: [{ id: 'ok', type: 'default', text: 'Отлично' }],
      }, () => navigate('/chats'));
      if (!twa?.showPopup) navigate('/chats');
    } catch (err) {
      twa?.HapticFeedback?.notificationOccurred('error');
      errBox.textContent = `Не получилось: ${err.message}`;
      errBox.style.display = 'block';
      btn.textContent = 'Подключить'; btn.disabled = false;
    }
  });

  return el;
}

/**
 * Settings for one connected bot: its global role (governs every chat of THIS
 * bot) and disconnect. Opened by tapping the bot's card on the chats screen.
 */
export function renderBotSettings(navigate, botId) {
  const el = document.createElement('div');
  el.className = 'screen';
  el.innerHTML = `
    <div class="chat-topbar">
      <div class="chat-topbar-info">
        <div class="chat-topbar-name">настройки бота</div>
        <div class="chat-topbar-mode" id="bs-sub" style="color:var(--hint)">…</div>
      </div>
    </div>
    <div class="scroll" id="bs-scroll">
      <div class="loading-row"><div class="spinner"></div></div>
    </div>
  `;

  const handleBack = () => navigate('/chats');
  twa?.BackButton?.show();
  twa?.BackButton?.onClick(handleBack);
  const obs = new MutationObserver(() => {
    if (!document.body.contains(el)) {
      twa?.BackButton?.offClick(handleBack);
      obs.disconnect();
    }
  });
  obs.observe(document.body, { childList: true, subtree: true });

  // The bot's global role governs every one of its chats. roleId 0 clears it.
  function roleSectionHTML(roles, activeRoleId) {
    const rows = [{ id: 0, name: 'без роли — обычный ассистент' }, ...roles];
    return `
      <div class="st-section">
        <p class="st-section-label">роль бота</p>
        <p style="font-size:12px;color:var(--muted);padding:0 4px 8px;line-height:1.4;">
          применяется ко всем чатам этого бота
        </p>
        <div class="st-card picker-list" id="bs-role-list" style="border-top:none;">
          ${rows.map(r => {
            const on = String(r.id) === String(activeRoleId || 0);
            return `
              <div class="picker-row ${on ? 'picker-active' : ''}" data-role-id="${r.id}">
                <span class="picker-row-icon">${on ? '◉' : '○'}</span>
                <span class="picker-row-name">${esc(r.name)}</span>
              </div>`;
          }).join('')}
        </div>
      </div>`;
  }

  function deliverySectionHTML(bot) {
    return `
      <div class="st-section">
        <p class="st-section-label">формат ответов</p>
        <div class="st-card" style="padding:14px;display:flex;flex-direction:column;gap:14px;">
          ${deliverySelectHTML('bs-rich-formatting', 'rich-форматирование', 'таблицы, списки и код в ответах роли', bot.rich_formatting_enabled)}
          ${deliverySelectHTML('bs-streaming', 'потоковый AI-режим', 'псевдо-стриминг готового безопасного ответа', bot.streaming_enabled)}
        </div>
      </div>`;
  }

  function render(bot, roles) {
    el.querySelector('#bs-sub').textContent = `@${bot.bot_username || bot.bot_id}`;
    let activeRoleId = bot.ai_role?.id || 0;

    el.querySelector('#bs-scroll').innerHTML = `
      <div class="st-content">
        <div class="st-section">
          <div class="bot-card" style="margin:0;">
            <div class="role-card-icon" style="background:var(--success-soft);">🤖</div>
            <div class="chat-item-body">
              <div class="role-card-name">@${esc(bot.bot_username || bot.bot_id)}</div>
              <div class="role-card-desc">статус: ${esc(bot.status || 'active')}</div>
            </div>
          </div>
        </div>
        <div id="bs-role-section">${roleSectionHTML(roles, activeRoleId)}</div>
        <div id="bs-delivery-section">${deliverySectionHTML(bot)}</div>
        <div class="st-section">
          <div class="st-card">
            <div class="st-row st-row-danger" id="btn-disconnect">
              <span class="st-row-icon">🔌</span>
              <div class="st-row-body">
                <p class="st-row-title" style="color:var(--danger)">отключить бота</p>
                <p class="st-row-desc">ИИ перестанет отвечать в его чатах; история останется</p>
              </div>
            </div>
          </div>
        </div>
      </div>`;

    function wireRolePicker() {
      el.querySelectorAll('#bs-role-list [data-role-id]').forEach(row => {
        row.addEventListener('click', async () => {
          const roleId = Number(row.dataset.roleId);
          if (roleId === activeRoleId) return;
          twa?.HapticFeedback?.selectionChanged();
          const roleName = roleId
            ? roles.find(r => Number(r.id) === roleId)?.name || 'выбранная роль'
            : 'обычный ассистент';
          const roleContextPolicy = await chooseBotRoleContextPolicy(roleName);
          if (!roleContextPolicy) return;
          const prev = activeRoleId;
          activeRoleId = roleId;
          el.querySelector('#bs-role-section').innerHTML = roleSectionHTML(roles, activeRoleId);
          wireRolePicker();
          try {
            bot = await api.setBotRole(bot.bot_id, roleId, roleContextPolicy);
            activeRoleId = bot.ai_role?.id || 0;
          } catch (err) {
            activeRoleId = prev;
            el.querySelector('#bs-role-section').innerHTML = roleSectionHTML(roles, activeRoleId);
            wireRolePicker();
            twa?.showAlert(`Ошибка: ${err.message}`);
          }
        });
      });
    }
    wireRolePicker();

    function wireDeliveryControls() {
      ['#bs-rich-formatting', '#bs-streaming'].forEach(selector => {
        el.querySelector(selector)?.addEventListener('change', async () => {
          twa?.HapticFeedback?.selectionChanged();
          try {
            bot = await api.updateBotSettings(bot.bot_id, {
              rich_formatting_enabled: modeToNullableBool(el.querySelector('#bs-rich-formatting').value),
              streaming_enabled:       modeToNullableBool(el.querySelector('#bs-streaming').value),
            });
            twa?.HapticFeedback?.notificationOccurred('success');
          } catch (err) {
            twa?.HapticFeedback?.notificationOccurred('error');
            twa?.showAlert(`Ошибка: ${err.message}`);
            el.querySelector('#bs-delivery-section').innerHTML = deliverySectionHTML(bot);
            wireDeliveryControls();
          }
        });
      });
    }
    wireDeliveryControls();

    el.querySelector('#btn-disconnect').addEventListener('click', () => {
      twa?.HapticFeedback?.impactOccurred('medium');
      twa?.showPopup({
        title: 'Отключить бота?',
        message: 'ИИ перестанет получать и отвечать на сообщения этого бота.',
        buttons: [
          { id: 'cancel', type: 'cancel', text: 'Отмена' },
          { id: 'off', type: 'destructive', text: 'Отключить' },
        ],
      }, async (btnId) => {
        if (btnId !== 'off') return;
        try {
          await api.disconnectBot(bot.bot_id);
          twa?.HapticFeedback?.notificationOccurred('success');
          navigate('/chats');
        } catch (err) {
          twa?.showAlert(`Ошибка: ${err.message}`);
        }
      });
    });
  }

  Promise.all([api.bots(), api.roles().catch(() => ({ roles: [] }))])
    .then(([botsData, rolesData]) => {
      const bot = (botsData.bots || []).find(b => String(b.bot_id) === String(botId));
      if (!bot) { navigate('/chats'); return; }
      render(bot, rolesData.roles || []);
    })
    .catch(err => {
      el.querySelector('#bs-scroll').innerHTML = `
        <div class="empty-state"><div class="empty-icon">⚠️</div><div class="empty-subtitle">${esc(err.message)}</div></div>`;
    });

  return el;
}
