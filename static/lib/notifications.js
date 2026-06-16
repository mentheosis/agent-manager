const IDLE_NOTIFY_PREFIX = 'am.idleNotifications.';

export function idleNotificationKey(title) {
    return `${IDLE_NOTIFY_PREFIX}${encodeURIComponent(title || '')}`;
}

export function idleNotificationsEnabled(title) {
    if (!title) return false;
    return localStorage.getItem(idleNotificationKey(title)) === '1';
}

export function setIdleNotificationsEnabled(title, enabled) {
    if (!title) return;
    const key = idleNotificationKey(title);
    if (enabled) {
        localStorage.setItem(key, '1');
    } else {
        localStorage.removeItem(key);
    }
}

export function notificationsSupported() {
    return 'Notification' in window;
}

export async function ensureNotificationPermission() {
    if (!notificationsSupported()) return 'unsupported';
    if (Notification.permission === 'default') {
        return await Notification.requestPermission();
    }
    return Notification.permission;
}

export function showIdleNotification(title, displayTitle) {
    if (!notificationsSupported() || Notification.permission !== 'granted') return;
    const name = displayTitle || title || 'Agent';
    new Notification(`${name} is idle`, {
        body: 'The agent finished its current turn.',
        tag: `agent-manager-idle-${title || name}`,
        renotify: false,
    });
}
