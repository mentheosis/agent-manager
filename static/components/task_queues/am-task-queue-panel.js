import { queueRequest } from "./api.js";
class AmTaskQueuePanel extends HTMLElement {
  constructor() {
    super();
    this._instance = null;
    this._timer = null;
    this._loading = false;
  }
  connectedCallback() {
    this.innerHTML = `<h3>Task queue</h3><p class="queue-state">Stopped</p><dl><dt>Available tasks in pool</dt><dd data-count="available_tasks">—</dd><dt>Active tasks here</dt><dd data-count="active_tasks">—</dd><dt>Active workers here</dt><dd data-count="workers">—</dd></dl><form><label>Max workers<input name="max-workers" type="number" min="1" value="1" required></label><button>Apply</button></form><form class="attempt-limits"><label>Lease seconds<input name="lease" type="number" min="15" required></label><label>Task time limit (seconds)<input name="seconds" type="number" min="15" required></label><label>Task token limit<input name="tokens" type="number" min="1" required></label><button>Apply task limits</button></form><p class="attempt-effective"></p><p class="queue-effective">Effective limit unavailable</p><div class="queue-controls"><button data-action="start">Start / resume</button><button data-action="pause">Pause dispatch</button><button data-action="stop">Drain and stop</button></div><p>Lowering the limit lets active workers finish. Pause stops this controller assigning work. Other controllers are independent.</p><p class="queue-error" role="alert"></p>`;
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
    if (inst) {
      this.load();
      this._timer = setInterval(() => this.load(), 3000);
    }
  }
  disconnectedCallback() {
    clearInterval(this._timer);
  }
  async load() {
    const title = this._instance?.title;
    if (!title || this._loading) return;
    this._loading = true;
    try {
      const data = await queueRequest(title, "status");
      if (title !== this._instance?.title) return;
      this.querySelector(".queue-state").textContent = data.draining
        ? "Draining"
        : `Controller ${data.state} · Pool ${data.queue?.queue_id || this._instance.queue_id || ""}`;
      const q = data.queue;
      for (const key of ["available_tasks", "active_tasks"])
        this.querySelector(`[data-count=${key}]`).textContent = q
          ? q[key]
          : "—";
      this.querySelector("[data-count=workers]").textContent = q
        ? `${q.active_workers} / ${q.max_workers}`
        : "—";
      this.querySelector(".queue-effective").textContent = q
        ? `Effective limit: ${q.max_workers}`
        : "Start the controller to read queue counts and settings.";
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
        this.querySelector(".queue-state").textContent =
          "Counts unavailable (connection lost)";
        this.querySelectorAll("[data-count]").forEach(
          (el) => (el.textContent = "—"),
        );
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
