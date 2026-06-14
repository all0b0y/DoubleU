import { api } from '/miniapp/api/client.js';

const twa = window.Telegram?.WebApp;

function esc(str) {
  return String(str || '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

/** Connect (or manage) the tenant's Telegram bot — the product's main channel. */
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

  function renderConnected(bot) {
    el.querySelector('#ab-scroll').innerHTML = `
      <div class="st-content">
        <div class="st-section">
          <div class="bot-card" style="margin:0;">
            <div class="role-card-icon" style="background:rgba(16,185,129,0.16);">🤖</div>
            <div class="chat-item-body">
              <div class="role-card-name">@${esc(bot.bot_username || bot.bot_id)}</div>
              <div class="role-card-desc">статус: ${esc(bot.status || 'active')}</div>
            </div>
          </div>
        </div>
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
          await api.disconnectBot();
          twa?.HapticFeedback?.notificationOccurred('success');
          navigate('/chats');
        } catch (err) {
          twa?.showAlert(`Ошибка: ${err.message}`);
        }
      });
    });
  }

  function renderForm() {
    el.querySelector('#ab-scroll').innerHTML = `
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
      </div>`;

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
        // Fallback when showPopup is unavailable (older clients / browser dev).
        if (!twa?.showPopup) navigate('/chats');
      } catch (err) {
        twa?.HapticFeedback?.notificationOccurred('error');
        errBox.textContent = `Не получилось: ${err.message}`;
        errBox.style.display = 'block';
        btn.textContent = 'Подключить'; btn.disabled = false;
      }
    });
  }

  api.getBot().then(bot => {
    if (bot.connected) renderConnected(bot);
    else renderForm();
  }).catch(() => renderForm());

  return el;
}
