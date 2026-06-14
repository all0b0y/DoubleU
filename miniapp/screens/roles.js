import { api } from '/miniapp/api/client.js';
import { renderBottomNav } from '/miniapp/components/nav.js';

const twa = window.Telegram?.WebApp;

function esc(str) {
  return String(str || '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

function roleCardHTML(role, usageCount) {
  const usage = usageCount > 0
    ? `<span class="role-card-usage">в ${usageCount} ${plural(usageCount, 'чате', 'чатах', 'чатах')}</span>`
    : `<span class="role-card-usage" style="color:var(--muted);background:none;">не назначена</span>`;
  return `
    <div class="role-card" data-role-id="${role.id}">
      <div class="role-card-icon">🎭</div>
      <div class="role-card-body">
        <div class="chat-item-row1">
          <div class="role-card-name">${esc(role.name)}</div>
          ${usage}
        </div>
        <div class="role-card-desc">${esc(role.description)}</div>
      </div>
    </div>
  `;
}

function plural(n, one, few, many) {
  const m10 = n % 10, m100 = n % 100;
  if (m10 === 1 && m100 !== 11) return one;
  if (m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14)) return few;
  return many;
}

// Ready-made personas: one tap opens the create form pre-filled, the user only
// reviews and saves. Kept client-side — they are templates, not tenant data.
export const ROLE_TEMPLATES = [
  {
    icon: '💼',
    name: 'Менеджер по продажам',
    description: 'Вежливый и уверенный менеджер по продажам. Отвечает от имени компании: помогает выбрать товар или услугу, отвечает на вопросы о ценах и сроках, мягко ведёт собеседника к покупке.',
    speaking_style: 'Дружелюбно и по делу, на «вы», короткие сообщения без канцелярита. Один уточняющий вопрос за раз.',
    goals: 'Понять потребность собеседника, предложить подходящий вариант и довести до заказа или встречи.',
    constraints: 'Не обещать скидок и сроков, которых нет в материалах роли. Не давить на собеседника. Спорные вопросы передавать владельцу.',
  },
  {
    icon: '🛟',
    name: 'Техподдержка',
    description: 'Терпеливый специалист техподдержки. Помогает разобраться с проблемой: уточняет детали, предлагает шаги решения, при необходимости обещает передать вопрос специалисту.',
    speaking_style: 'Спокойно и структурно, на «вы». Шаги решения — нумерованным списком. Без жаргона.',
    goals: 'Решить проблему собеседника за минимальное число сообщений или корректно эскалировать её владельцу.',
    constraints: 'Не выдумывать решения, которых нет в материалах роли. Не обвинять пользователя. Не обсуждать внутренние процессы компании.',
  },
];

function templateCardHTML(t, i) {
  return `
    <div class="role-card" data-template="${i}" style="background:transparent;border:1px dashed var(--border-color);">
      <div class="role-card-icon" style="background:var(--surface-active);">${t.icon}</div>
      <div class="role-card-body">
        <div class="chat-item-row1">
          <div class="role-card-name">${esc(t.name)}</div>
          <span class="role-card-usage">использовать</span>
        </div>
        <div class="role-card-desc">${esc(t.description)}</div>
      </div>
    </div>
  `;
}

export function renderRoles(navigate) {
  const el = document.createElement('div');
  el.className = 'screen';
  el.innerHTML = `
    <div class="chats-header">
      <div class="chats-header-left">
        <span class="chats-header-brand">DoubleU</span>
        <span class="chats-header-title">Роли</span>
      </div>
      <button class="chats-header-add" id="btn-new-role" aria-label="Создать роль">+</button>
    </div>

    <div class="scroll" id="role-list">
      <div class="loading-row"><div class="spinner"></div></div>
    </div>
  `;
  el.appendChild(renderBottomNav('roles', navigate));

  twa?.BackButton?.hide();

  el.querySelector('#btn-new-role').addEventListener('click', () => {
    twa?.HapticFeedback?.impactOccurred('light');
    navigate('/roles/new');
  });

  async function load() {
    const list = el.querySelector('#role-list');
    try {
      const [rolesData, chatsData] = await Promise.all([api.roles(), api.chats()]);
      const roles = rolesData.roles || [];
      const chats = chatsData.chats || [];
      const usage = {};
      for (const c of chats) {
        if (c.ai_role_id) usage[c.ai_role_id] = (usage[c.ai_role_id] || 0) + 1;
      }

      // Templates are offered until the user has created their own roles with
      // those names; existing names are filtered out to avoid 409 dead-ends.
      const existingNames = new Set(roles.map(r => r.name.toLowerCase()));
      const templates = ROLE_TEMPLATES.filter(t => !existingNames.has(t.name.toLowerCase()));
      const templatesHTML = templates.length ? `
        <div class="section-header" style="margin-top:${roles.length ? '12px' : '0'};">
          <span class="section-title">Готовые роли</span>
          <span style="font-size:11px;color:var(--muted);">настрой под себя</span>
        </div>
        <div style="display:flex;flex-direction:column;gap:8px;padding:4px 0 16px;">
          ${templates.map((t, i) => templateCardHTML(t, ROLE_TEMPLATES.indexOf(t))).join('')}
        </div>` : '';

      if (!roles.length) {
        list.innerHTML = `
          <div class="empty-state" style="padding:24px 24px 12px;">
            <div class="empty-icon">🎭</div>
            <div class="empty-title">Ролей пока нет</div>
            <div class="empty-subtitle">
              Роль — это персонаж, от лица которого ИИ отвечает в чате.
              Возьми готовую роль ниже или создай свою с нуля.
            </div>
            <button class="btn btn-secondary" id="btn-first-role" style="max-width:260px;margin-top:8px;">
              Создать с нуля
            </button>
          </div>
          ${templatesHTML}`;
        list.querySelector('#btn-first-role').addEventListener('click', () => {
          twa?.HapticFeedback?.impactOccurred('medium');
          navigate('/roles/new');
        });
      } else {
        list.innerHTML = `
          <div style="display:flex;flex-direction:column;gap:8px;padding:4px 0 8px;">
            ${roles.map(r => roleCardHTML(r, usage[r.id] || 0)).join('')}
          </div>
          <div style="padding:0 20px;font-size:12px;color:var(--muted);line-height:1.5;">
            Нажми на роль, чтобы изменить её или выбрать чаты, где она отвечает.
          </div>
          ${templatesHTML}`;
      }

      list.querySelectorAll('[data-role-id]').forEach(card => {
        card.addEventListener('click', () => {
          twa?.HapticFeedback?.selectionChanged();
          navigate(`/roles/${card.dataset.roleId}`);
        });
      });

      // Template tap → create form pre-filled with the persona.
      list.querySelectorAll('[data-template]').forEach(card => {
        card.addEventListener('click', () => {
          twa?.HapticFeedback?.selectionChanged();
          const t = ROLE_TEMPLATES[Number(card.dataset.template)];
          sessionStorage.setItem('role_template', JSON.stringify(t));
          navigate('/roles/new');
        });
      });
    } catch (err) {
      list.innerHTML = `
        <div class="empty-state">
          <div class="empty-icon">⚠️</div>
          <div class="empty-title">Ошибка</div>
          <div class="empty-subtitle">${esc(err.message)}</div>
        </div>`;
    }
  }

  load();
  return el;
}
