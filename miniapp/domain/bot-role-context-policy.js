const BOT_ROLE_CONTEXT_POLICY_OPTIONS = Object.freeze([
  {
    value: 'reuse_role_context',
    buttonType: 'default',
    buttonText: 'Использовать память',
  },
  {
    value: 'new_role_context',
    buttonType: 'destructive',
    buttonText: 'Начать чистую',
  },
]);

const BOT_ROLE_CONTEXT_POLICY_VALUES = new Set(
  BOT_ROLE_CONTEXT_POLICY_OPTIONS.map((option) => option.value),
);

export function isBotRoleContextPolicy(value) {
  return BOT_ROLE_CONTEXT_POLICY_VALUES.has(value);
}

export function chooseBotRoleContextPolicy(twa, roleName) {
  return new Promise((resolve) => {
    const message = `Выбери память для "${roleName}".`;
    if (twa?.showPopup) {
      twa.showPopup({
        title: 'Память роли',
        message,
        buttons: [
          ...BOT_ROLE_CONTEXT_POLICY_OPTIONS.map((option) => ({
            id: option.value,
            type: option.buttonType,
            text: option.buttonText,
          })),
          { id: 'cancel', type: 'cancel', text: 'Отмена' },
        ],
      }, (btnId) => {
        resolve(isBotRoleContextPolicy(btnId) ? btnId : null);
      });
      return;
    }

    const allowed = BOT_ROLE_CONTEXT_POLICY_OPTIONS
      .map((option) => option.value)
      .join(' или ');
    const choice = window.prompt(`${message}\n\nВведите ${allowed}.`, '')?.trim();
    resolve(isBotRoleContextPolicy(choice) ? choice : null);
  });
}
