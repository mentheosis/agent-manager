import "./am-task-queue-loader.js";
import { queueRequest, textElement } from "./api.js";
import { appendQueueLogs } from "./am-task-queue-activity.js";
class AmTaskQueueTasks extends HTMLElement {
  constructor() {
    super();
    this._instance = null;
    this._timer = null;
    this._after = 0;
    this._offset = 0;
    this._busy = false;
    this._filters = {};
    this._onFilters = (e) => {
      if (e.detail.scope !== "task_queue") return;
      this._filters = e.detail.filters;
      this.querySelectorAll(".loop-event").forEach(
        (row) => (row.hidden = this._filters[row.dataset.eventType] === false),
      );
    };
  }
  connectedCallback() {
    this.classList.add("controller-activity");
    this.innerHTML = `<header class="queue-heading"><h2>Task queue</h2><nav><button data-view="tasks">Tasks</button><button data-view="activity">Activity</button><button class="queue-load">Load tasks</button></nav><p class="queue-error" role="alert"></p></header><section class="queue-task-list"><div class="queue-task-rows"></div><button class="queue-prev">Previous</button><button class="queue-next">Next</button></section><section class="loop-events" hidden></section><am-task-queue-loader></am-task-queue-loader>`;
    this.querySelectorAll("[data-view]").forEach((b) =>
      b.addEventListener("click", () => {
        this.querySelector(".queue-task-list").hidden =
          b.dataset.view !== "tasks";
        this.querySelector(".loop-events").hidden =
          b.dataset.view !== "activity";
      }),
    );
    this.querySelector(".queue-load").onclick = () => this.querySelector("am-task-queue-loader").open(this._instance);
    this.querySelector(".queue-prev").onclick = () => {
      this._offset = Math.max(0, this._offset - 100);
      this.load();
    };
    this.querySelector(".queue-next").onclick = () => {
      this._offset += 100;
      this.load();
    };
    document.addEventListener("filter-changed", this._onFilters);
  }
  set instance(inst) {
    if (this._instance?.title === inst?.title) return;
    clearInterval(this._timer);
    const loader = this.querySelector("am-task-queue-loader");
    loader?.querySelector("dialog")?.close();
    loader?.invalidate();
    this._instance = inst;
    this._after = 0;
    this._offset = 0;
    this._signature = "";
    this.querySelector(".loop-events").replaceChildren();
    this.querySelector(".queue-task-rows").replaceChildren();
    if (inst) {
      this.load();
      this._timer = setInterval(() => this.load(), 3000);
    }
  }
  disconnectedCallback() {
    clearInterval(this._timer);
    document.removeEventListener("filter-changed", this._onFilters);
  }
  async load() {
    const title = this._instance?.title;
    if (!title || this._busy) return;
    this._busy = true;
    try {
      const [tasks, logs] = await Promise.all([
        queueRequest(title, `tasks?offset=${this._offset}`),
        queueRequest(title, `logs?after=${this._after}`),
      ]);
      if (title !== this._instance?.title) return;
      const signature = JSON.stringify(tasks);
      if (signature !== this._signature) {
        this.renderTasks(tasks);
        this._signature = signature;
      }
      appendQueueLogs(
        this.querySelector(".loop-events"),
        logs,
        title,
        this._filters,
      );
      if (logs.length) this._after = logs[logs.length - 1].id;
      this.querySelector(".queue-error").textContent = "";
      this.querySelector(".queue-prev").disabled = this._offset === 0;
      this.querySelector(".queue-next").disabled = tasks.length < 100;
    } catch (error) {
      if (title === this._instance?.title)
        this.querySelector(".queue-error").textContent = error.message;
    } finally {
      this._busy = false;
    }
  }
  renderTasks(tasks) {
    const rows = this.querySelector(".queue-task-rows");
    const open = new Set(
      [...rows.querySelectorAll("details[open]")].map((el) => el.dataset.task),
    );
    rows.replaceChildren();
    if (!tasks.length) {
      rows.append(
        textElement(
          "p",
          "No tasks on this page. Use Load tasks to preview and insert a task batch.",
        ),
      );
      return;
    }
    for (const task of tasks) {
      const row = document.createElement("details");
      row.dataset.task = String(task.id);
      row.open = open.has(String(task.id));
      row.append(
        textElement(
          "summary",
          `#${task.id} · ${task.task_type} · ${task.status} · attempt ${task.attempt_count}/${task.max_attempts}`,
        ),
      );
      row.append(
        textElement(
          "pre",
          JSON.stringify(
            {
              workflow: task.workflow_id,
              task_type: task.task_type,
              parameters: task.parameters,
              reason: task.latest_error,
              usage: task.latest_usage,
              result: task.latest_result || task.accepted_result,
            },
            null,
            2,
          ),
        ),
      );
      const actions =
        task.status === "awaiting_review"
          ? ["approve", "reject"]
          : ["failed", "blocked"].includes(task.status)
            ? ["retry"]
            : ["completed", "cancelled"].includes(task.status)
              ? []
              : ["cancel"];
      for (const action of actions) {
        const b = textElement("button", action);
        b.onclick = async () => {
          b.disabled = true;
          try {
            await queueRequest(this._instance.title, "control", {
              action,
              task_id: task.id,
              attempt_id: task.latest_attempt,
            });
            await this.load();
          } catch (error) {
            this.querySelector(".queue-error").textContent = error.message;
          } finally {
            b.disabled = false;
          }
        };
        row.append(b);
      }
      for (const artifact of (task.latest_result || task.accepted_result)
        ?.artifacts || []) {
        const link = textElement("a", artifact.path);
        link.href = `/api/task-queues/${encodeURIComponent(this._instance.title)}/artifacts/${artifact.sha256}`;
        link.target = "_blank";
        link.rel = "noopener";
        row.append(link);
      }
      rows.append(row);
    }
  }
}
customElements.define("am-task-queue-tasks", AmTaskQueueTasks);
