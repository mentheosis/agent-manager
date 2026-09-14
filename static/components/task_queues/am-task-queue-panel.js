import { queueRequest } from "./api.js";
class AmTaskQueuePanel extends HTMLElement {
  constructor() {
    super();
    this._instance = null;
    this._timer = null;
    this._loading = false;
    this._onTasks = e => {
      if (e.detail.title !== this._instance?.title) return;
      this.showTaskCounts(e.detail.tasks);
    };
  }
  connectedCallback() {
    document.addEventListener("queue-tasks-updated", this._onTasks);
    this.innerHTML = `<div class="queue-resize-handle" role="separator" aria-label="Resize task queue panel" aria-orientation="vertical" tabindex="0"></div><div class="queue-panel-content"><section class="queue-controller-section"><h3>Controller</h3><p class="queue-state queue-status-badge" data-state="unknown">Checking controller…</p><div class="queue-controls"><button data-action="start">Start / resume</button><button data-action="pause">Pause dispatch</button><button data-action="stop">Drain and stop</button></div><p class="queue-help">Pause stops new assignments. Drain lets active workers finish before stopping.</p><p class="queue-error" role="alert"></p></section><section><h3>Task activity</h3><dl><dt>Queued</dt><dd data-count="queued">—</dd><dt>Active</dt><dd data-count="active">—</dd><dt>Completed</dt><dd data-count="completed">—</dd><dt>Other</dt><dd data-count="other">—</dd></dl></section><section><h3>Worker capacity</h3><dl><dt>Active workers here</dt><dd data-count="workers">—</dd></dl><form><label>Max workers<input name="max-workers" type="number" min="1" value="1" required></label><button>Apply</button></form><p class="queue-effective">Effective limit unavailable</p><p class="queue-help">Lowering the limit lets active workers finish. Each controller has its own limit.</p></section><section><h3>Task limits</h3><form class="attempt-limits"><label>Lease seconds<input name="lease" type="number" min="15" required></label><label>Task time limit (seconds)<input name="seconds" type="number" min="15" required></label><label>Task token limit<input name="tokens" type="number" min="1" required></label><button>Apply task limits</button></form><p class="attempt-effective"></p></section></div>`;
    const handle = this.querySelector(".queue-resize-handle");
    const resize = width => {
      const maximum = Math.max(260, Math.min(720, window.innerWidth - 320));
      const bounded = Math.max(260, Math.min(maximum, width));
      this.style.width = `${bounded}px`;
      handle.setAttribute("aria-valuemin", "260");
      handle.setAttribute("aria-valuemax", String(maximum));
      handle.setAttribute("aria-valuenow", String(Math.round(bounded)));
    };
    resize(this.getBoundingClientRect().width || 312);
    let drag = null;
    handle.addEventListener("pointerdown", e => {
      if (e.button !== 0) return;
      drag = { x: e.clientX, width: this.getBoundingClientRect().width };
      handle.setPointerCapture(e.pointerId);
      e.preventDefault();
    });
    handle.addEventListener("pointermove", e => {
      if (drag) resize(drag.width + drag.x - e.clientX);
    });
    const finish = () => { drag = null; };
    handle.addEventListener("pointerup", finish);
    handle.addEventListener("pointercancel", finish);
    handle.addEventListener("lostpointercapture", finish);
    handle.addEventListener("keydown", e => {
      if (!["ArrowLeft", "ArrowRight"].includes(e.key)) return;
      e.preventDefault();
      resize(this.getBoundingClientRect().width + (e.key === "ArrowLeft" ? 20 : -20));
    });
    this.querySelector("form").addEventListener("submit", (e) => {
      e.preventDefault();
      this.control("max_workers");
    });
    this.querySelector(".attempt-limits").addEventListener("submit", (e) => { e.preventDefault(); this.control("limits"); });
    this.querySelectorAll("[data-action]").forEach((button) =>
      button.addEventListener("click", () =>
        this.control(button.dataset.action),
      ),
    );
  }
  set instance(inst) {
    if (this._instance?.title === inst?.title) return;
    clearInterval(this._timer);
    this._instance = inst;
    this.classList.toggle("visible", !!inst);
    this._initialized = false;
    this.showTaskCounts(null);
    this.publishStatus("unknown");
    if (inst) {
      this.load();
      this._timer = setInterval(() => this.load(), 3000);
    }
  }
  disconnectedCallback() {
    document.removeEventListener("queue-tasks-updated", this._onTasks);
    clearInterval(this._timer);
  }
  publishStatus(value) {
    const state = ["running", "paused", "draining", "stopped", "unavailable"].includes(value) ? value : "unknown";
    const label = state === "unknown" ? "Checking controller…" : `Controller ${state}`;
    const badge = this.querySelector(".queue-state");
    badge.textContent = label;
    badge.setAttribute("data-state", state);
    document.dispatchEvent(new CustomEvent("queue-controller-status", { detail: { title: this._instance?.title, state, label } }));
  }
  showTaskCounts(tasks) {
    const counts = { queued: 0, active: 0, completed: 0, other: 0 };
    for (const task of tasks || []) {
      const category = ["claimed", "running", "submitted"].includes(task.status) ? "active"
        : ["queued", "completed"].includes(task.status) ? task.status : "other";
      counts[category]++;
    }
    for (const [key, value] of Object.entries(counts))
      this.querySelector(`[data-count=${key}]`).textContent = tasks ? value : "—";
  }
  async load() {
    const title = this._instance?.title;
    if (!title || this._loading) return;
    this._loading = true;
    try {
      const data = await queueRequest(title, "status");
      if (title !== this._instance?.title) return;
      this.publishStatus(data.draining ? "draining" : data.state);
      const q = data.queue || data.settings;
      this.querySelector("[data-count=workers]").textContent = q
        ? `${q.active_workers ?? 0} / ${q.max_workers}`
        : "—";
      this.querySelector(".queue-effective").textContent = q
        ? `${data.queue ? "Effective limit" : "Configured worker limit"}: ${q.max_workers}`
        : "Queue settings unavailable.";
      if (q && !this._initialized) {
        this.querySelector("input").value = q.max_workers;
        if (q.limits) for (const [name, key] of [["lease", "lease_secs"], ["seconds", "task_limit_secs"], ["tokens", "task_limit_tokens"]])
          this.querySelector(`[name=${name}]`).value = q.limits[key];
        this._initialized = true;
      }
      this.querySelector(".attempt-effective").textContent = q?.limits
        ? `New tasks: lease ${q.limits.lease_secs}s · time ${q.limits.task_limit_secs}s · tokens ${q.limits.task_limit_tokens}. Retries retain original limits.` : "";

    } catch (error) {
      if (title === this._instance?.title) {
        this.publishStatus("unavailable");
        this.querySelector("[data-count=workers]").textContent = "—";
        this.querySelector(".queue-error").textContent = error.message;
      }
    } finally {
      this._loading = false;
    }
  }
  async control(action) {
    const title = this._instance?.title;
    if (!title) return;
    const buttons = [...this.querySelectorAll("button")];
    buttons.forEach((b) => (b.disabled = true));
    try {
      if (action === "start") {
        await queueRequest(title, "start", {});
        await queueRequest(title, "control", { action: "resume" });
      } else {
        const body = { action };
        if (action === "max_workers")
          body.max_workers = Number(this.querySelector("input").value);
        if (action === "limits") body.limits = {
          lease_secs: Number(this.querySelector("[name=lease]").value),
          task_limit_secs: Number(this.querySelector("[name=seconds]").value),
          task_limit_tokens: Number(this.querySelector("[name=tokens]").value),
        };
        await queueRequest(title, "control", body);
      }
      this.querySelector(".queue-error").textContent = "";
      await this.load();
      document.dispatchEvent(
        new CustomEvent("queue-updated", { detail: { title } }),
      );
    } catch (error) {
      this.querySelector(".queue-error").textContent = error.message;
    } finally {
      buttons.forEach((b) => (b.disabled = false));
    }
  }
}
customElements.define("am-task-queue-panel", AmTaskQueuePanel);
