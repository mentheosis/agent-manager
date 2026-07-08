/**
 * Auth status banner - shows when disconnected or when the default provider is not authenticated.
 *
 * Modes (mutually exclusive, disconnected takes priority):
 *   disconnected — orange banner, "Reconnect" button
 *   unauthed     — red banner, "Log in" button
 *   (hidden)     — connected and authed
 */

class AmAuthBanner extends HTMLElement {
    constructor() {
        super();
        this._authed = true;
        this._disconnected = false;
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
            this.dispatchEvent(new CustomEvent('open-login-dialog', { bubbles: true }));
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

    currentMode() {
        if (this._disconnected) return 'disconnected';
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

        if (mode === 'disconnected') {
            this.hidden = false;
            this.dataset.mode = 'disconnected';
            this.querySelector('#auth-banner-text').textContent = 'Disconnected — reconnecting…';
            this.querySelector('#login-btn').hidden = true;
            this.querySelector('#reconnect-btn').hidden = false;
        } else if (mode === 'unauthed') {
            this.hidden = false;
            this.dataset.mode = 'unauthed';
            this.querySelector('#auth-banner-text').textContent = 'No configured provider appears authenticated in the container.';
            this.querySelector('#login-btn').hidden = false;
            this.querySelector('#reconnect-btn').hidden = true;
        }
    }
}

customElements.define('am-auth-banner', AmAuthBanner);
