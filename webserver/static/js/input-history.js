// InputHistory manages prompt input with history recall via prev/next buttons.
export class InputHistory {
  constructor(inputEl, onSend) {
    this.inputEl = inputEl;
    this.onSend = onSend;
    this.history = [];
    this.index = -1;
    this.draft = '';
    this.onNavigate = null; // callback to update button states

    this.inputEl.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        this.send();
      }
    });
  }

  send() {
    const text = this.inputEl.value.trim();
    if (!text) return;
    this.history.push(text);
    this.index = -1;
    this.draft = '';
    this.inputEl.value = '';
    this.onSend(text);
    this._notify();
  }

  loadHistory(items) {
    if (items && items.length) {
      this.history = [...items];
    } else {
      this.history = [];
    }
    this.index = -1;
    this.draft = '';
    this._notify();
  }

  prev() {
    if (this.history.length === 0) return;
    if (this.index === -1) {
      this.draft = this.inputEl.value;
      this.index = this.history.length - 1;
    } else if (this.index > 0) {
      this.index--;
    }
    this.inputEl.value = this.history[this.index];
    this.inputEl.focus();
    this._notify();
  }

  next() {
    if (this.index === -1) return;
    if (this.index < this.history.length - 1) {
      this.index++;
      this.inputEl.value = this.history[this.index];
    } else {
      this.index = -1;
      this.inputEl.value = this.draft;
    }
    this.inputEl.focus();
    this._notify();
  }

  get hasPrev() {
    if (this.history.length === 0) return false;
    if (this.index === -1) return true; // can go back
    return this.index > 0;
  }

  get hasNext() {
    return this.index !== -1;
  }

  _notify() {
    if (this.onNavigate) this.onNavigate();
  }
}
