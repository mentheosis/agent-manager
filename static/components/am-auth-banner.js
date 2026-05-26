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
    }

    connectedCallback() {
        this.id = 'auth-banner';
        this.hidden = true;
        this.innerHTML = `
            <span id="auth-banner-text"></span>
            <button id="login-btn" type="button">Log in</button>
            <button id="reconnect-btn" type="button">Reconnect</button>
        `;

        this.querySelector('#login-btn').addEventListener('click', () => {
            this.dispatchEvent(new CustomEvent('open-login-dialog', { bubbles: true }));
        });

        this.querySelector('#reconnect-btn').addEventListener('click', () => {
            this.dispatchEvent(new CustomEvent('reconnect-requested', { bubbles: true }));
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

    _update() {
        if (!this.isConnected) return;

        if (this._disconnected) {
            this.hidden = false;
            this.dataset.mode = 'disconnected';
            this.querySelector('#auth-banner-text').textContent = 'Disconnected — reconnecting…';
            this.querySelector('#login-btn').hidden = true;
            this.querySelector('#reconnect-btn').hidden = false;
        } else if (!this._authed) {
            this.hidden = false;
            this.dataset.mode = 'unauthed';
            this.querySelector('#auth-banner-text').textContent = 'Default provider is not authenticated in the container.';
            this.querySelector('#login-btn').hidden = false;
            this.querySelector('#reconnect-btn').hidden = true;
        } else {
            this.hidden = true;
            delete this.dataset.mode;
        }
    }
}

customElements.define('am-auth-banner', AmAuthBanner);
