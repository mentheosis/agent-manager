/**
 * Auth status banner - shows when Claude is not authenticated.
 */

class AmAuthBanner extends HTMLElement {
    constructor() {
        super();
        this._authed = true;
    }

    connectedCallback() {
        this.id = 'auth-banner';
        this.hidden = true;
        this.innerHTML = `
            <span>Claude is not authenticated in the container.</span>
            <button id="login-btn" type="button">Log in</button>
        `;

        this.querySelector('#login-btn').addEventListener('click', () => {
            this.dispatchEvent(new CustomEvent('open-login-dialog', { bubbles: true }));
        });
    }

    get authed() {
        return this._authed;
    }

    set authed(value) {
        this._authed = value;
        this.hidden = value;
    }
}

customElements.define('am-auth-banner', AmAuthBanner);
