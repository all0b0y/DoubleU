// Shared bottom navigation — the single source of truth for root tabs.
// Маркет (Iteration 2) intentionally absent: every visible item must work.

const twa = window.Telegram?.WebApp;

const NAV_ITEMS = [
  { id: 'chats', icon: '💬', label: 'чаты', path: '/chats' },
  { id: 'roles', icon: '🎭', label: 'роли', path: '/roles' },
  { id: 'me',    icon: '👤', label: 'я',    path: '/me' },
];

/**
 * Returns a wired-up <nav> element for root screens.
 * @param {string} active - id of the current tab ('chats' | 'roles' | 'me')
 * @param {(path: string) => void} navigate - router navigate function
 */
export function renderBottomNav(active, navigate) {
  const nav = document.createElement('nav');
  nav.className = 'bottom-nav';
  nav.innerHTML = NAV_ITEMS.map(item => `
    <button class="bottom-nav-item ${item.id === active ? 'active' : ''}" data-nav="${item.id}">
      <span class="bottom-nav-icon">${item.icon}</span>
      <span class="bottom-nav-label">${item.label}</span>
    </button>
  `).join('');

  nav.querySelectorAll('[data-nav]').forEach(btn => {
    btn.addEventListener('click', () => {
      const item = NAV_ITEMS.find(i => i.id === btn.dataset.nav);
      if (!item || item.id === active) return;
      twa?.HapticFeedback?.selectionChanged();
      navigate(item.path);
    });
  });

  return nav;
}
