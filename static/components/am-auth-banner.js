/**
 * Auth status banner - shows when disconnected, when the session has expired,
 * or when no provider is authenticated.
 *
 * Modes (mutually exclusive, evaluated in priority order):
 *   disconnected — orange banner, "Reconnect" button
 *   expired      — red banner, "Re-authenticate" button (credentials were
 *                  rejected at runtime even though the CLI still reports authed)
 *   unauthed     — red banner, "Log in" button (never authenticated)
 *   (hidden)     — connected and authed
 *
 * Each mode can be dismissed via the × button; dismissal persists in
 * localStorage keyed to the mode so re-entering that mode later stays hidden
 * until the user re-triggers it explicitly.
 */

class AmAuthBanner extends HTMLElement {
    constructor() {
        super();
        this._authed = true;
        this._disconnected = false;
        this._needsReauth = false;
        this._reauthReason = null;
        this._storageKey = 'agent-manager.auth-banner.dismissed-mode';
        this._dismissedMode = null;
    }

    connectedCallback() {
        this.id = 'auth-banner';
        this.hidden = true;
        this.innerHTML = `
            <span id="auth-banner-text"></span>
            <button id="login-btn" type="button">Log in</button>
            <button id="reconnect-btn" type="button">Reconnect</button>
            <button id="dismiss-btn" type="button" title="Dismiss" aria-label="Dismiss">×</button>
        `;

        this.querySelector('#login-btn').addEventListener('click', () => {
            this.dispatchEvent(new CustomEvent('open-login-dialog', {
                bubbles: true,
                detail: { provider: 'claude' },
            }));
        });

        this.querySelector('#reconnect-btn').addEventListener('click', () => {
            this.dispatchEvent(new CustomEvent('reconnect-requested', { bubbles: true }));
        });

        this.querySelector('#dismiss-btn').addEventListener('click', () => {
            const mode = this.currentMode();
            if (mode) this.setDismissedMode(mode);
            this._update();
        });

        this._update();
    }

    get authed() {
        return this._authed;
    }

    set authed(value) {
        this._authed = value;
        this._update();
    }

    get disconnected() {
        return this._disconnected;
    }

    set disconnected(value) {
        this._disconnected = value;
        this._update();
    }

    get needsReauth() {
        return this._needsReauth;
    }

    set needsReauth(value) {
        this._needsReauth = Boolean(value);
        this._update();
    }

    // Optional machine reason ("expired" | "gateway") used to tailor the copy.
    set reauthReason(value) {
        this._reauthReason = value || null;
        this._update();
    }

    currentMode() {
        if (this._disconnected) return 'disconnected';
        if (this._needsReauth) return 'expired';
        if (!this._authed) return 'unauthed';
        return null;
    }

    isDismissed(mode) {
        return mode && this.getDismissedMode() === mode;
    }

    getDismissedMode() {
        try {
            return localStorage.getItem(this._storageKey) || this._dismissedMode;
        } catch {
            return this._dismissedMode;
        }
    }

    setDismissedMode(mode) {
        this._dismissedMode = mode;
        try {
            localStorage.setItem(this._storageKey, mode);
        } catch {
            // Ignore storage failures; the current click still hides via update.
        }
    }

    clearDismissedMode() {
        this._dismissedMode = null;
        try {
            localStorage.removeItem(this._storageKey);
        } catch {
            // Ignore storage failures.
        }
    }

    _update() {
        if (!this.isConnected) return;

        const mode = this.currentMode();
        if (!mode) {
            this.clearDismissedMode();
            this.hidden = true;
            delete this.dataset.mode;
            return;
        }

        if (this.isDismissed(mode)) {
            this.hidden = true;
            this.dataset.mode = mode;
            return;
        }

        this.querySelector('#dismiss-btn').hidden = false;
        const text = this.querySelector('#auth-banner-text');
        const loginBtn = this.querySelector('#login-btn');
        const reconnectBtn = this.querySelector('#reconnect-btn');

        this.hidden = false;
        this.dataset.mode = mode;

        if (mode === 'disconnected') {
            text.textContent = 'Disconnected — reconnecting…';
            loginBtn.hidden = true;
            reconnectBtn.hidden = false;
        } else if (mode === 'expired') {
            text.textContent = this._reauthReason === 'gateway'
                ? 'Claude requests are failing (gateway error) — your session may have expired. Re-authenticate to recover.'
                : 'Your Claude session has expired — re-authenticate to keep working.';
            loginBtn.hidden = false;
            loginBtn.textContent = 'Re-authenticate';
            reconnectBtn.hidden = true;
        } else if (mode === 'unauthed') {
            text.textContent = 'No configured provider appears authenticated in the container.';
            loginBtn.hidden = false;
            loginBtn.textContent = 'Log in';
            reconnectBtn.hidden = true;
        }
    }
}

customElements.define('am-auth-banner', AmAuthBanner);
