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
    this._expandedByController = new Map();
    this._onStatus = e => {
      if (e.detail.title !== this._instance?.title) return;
      const badge = this.querySelector(".queue-main-status");
      badge.textContent = e.detail.label;
      badge.setAttribute("data-state", e.detail.state);
    };
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
    this.innerHTML = `<header class="queue-heading"><h2>Task queue</h2><span class="queue-main-status queue-status-badge" data-state="unknown">Checking controller…</span><p class="queue-identity"></p><nav><div role="tablist" aria-label="Queue views"><button role="tab" aria-selected="true" data-view="tasks">Tasks</button><button role="tab" aria-selected="false" data-view="activity">Activity</button></div><button class="queue-load">Load tasks</button></nav><p class="queue-error" role="alert"></p></header><section class="queue-task-list"><div class="queue-task-rows"></div><button class="queue-prev">Previous</button><button class="queue-next">Next</button></section><section class="loop-events" hidden></section><am-task-queue-loader></am-task-queue-loader>`;
    this.querySelectorAll("[data-view]").forEach((b) =>
      b.addEventListener("click", () => {
        this.querySelectorAll("[data-view]").forEach(tab => tab.setAttribute("aria-selected", String(tab === b)));
        this.querySelector(".queue-task-list").hidden =
          b.dataset.view !== "tasks";
        this.querySelector(".loop-events").hidden =
          b.dataset.view !== "activity";
        if (b.dataset.view === "activity") this.scrollToBottom();
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
    document.addEventListener("queue-controller-status", this._onStatus);
  }
  set instance(inst) {
    if (this._instance?.title === inst?.title) return;
    clearInterval(this._timer);
    const loader = this.querySelector("am-task-queue-loader");
    loader?.querySelector("dialog")?.close();
    loader?.invalidate();
    this._instance = inst;
    this._onStatus({ detail: { title: inst?.title, state: "unknown", label: "Checking controller…" } });
    this.querySelector(".queue-identity").textContent = inst ? `queue_id: ${inst.queue_profile || inst.queue_id || ""}` : "";
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
    document.removeEventListener("queue-controller-status", this._onStatus);
  }
  async load() {
    const title = this._instance?.title;
    if (!title || this._busy) return;
    this._busy = true;
    try {
      const [tasks, logs] = await Promise.all([
        this.loadAllTasks(title),
        queueRequest(title, `logs?after=${this._after}`),
      ]);
      if (title !== this._instance?.title) return;
      document.dispatchEvent(new CustomEvent("queue-tasks-updated", { detail: { title, tasks } }));
      const signature = JSON.stringify(tasks);
      if (signature !== this._signature) {
        this.renderTasks(tasks);
        this._signature = signature;
      }
      const output = this.querySelector(".loop-events");
      const follow = output.scrollHeight - output.scrollTop - output.clientHeight < 100;
      appendQueueLogs(
        output,
        logs,
        title,
        this._filters,
      );
      if (logs.length) {
        this._after = logs[logs.length - 1].id;
        if (follow && !output.hidden) this.scrollToBottom();
      }
      this.querySelector(".queue-error").textContent = "";
      this.querySelector(".queue-prev").hidden = true;
      this.querySelector(".queue-next").hidden = true;
    } catch (error) {
      if (title === this._instance?.title) {
        document.dispatchEvent(new CustomEvent("queue-tasks-updated", { detail: { title, tasks: null } }));
        this.querySelector(".queue-error").textContent = error.message;
      }
    } finally {
      this._busy = false;
    }
  }
  scrollToBottom() {
    const activity = this.querySelector(".loop-events");
    const output = activity.hidden ? this.querySelector(".queue-task-list") : activity;
    output.scrollTop = output.scrollHeight;
  }
  async loadAllTasks(title) {
    const tasks = [];
    for (let offset = 0; ; offset += 100) {
      const page = await queueRequest(title, `tasks?offset=${offset}`);
      if (title !== this._instance?.title) return [];
      tasks.push(...page);
      if (page.length < 100) return tasks;
    }
  }
  expandableRow(body, values, key, open) {
    const header = document.createElement("tr");
    header.className = "queue-expand-row";
    header.dataset.expansion = key;
    const caretCell = document.createElement("td");
    const caret = textElement("button", "▸");
    caret.className = "queue-caret";
    caret.setAttribute("aria-label", `Expand ${values[0]}`);
    caretCell.append(caret); header.append(caretCell);
    values.forEach(value => header.append(textElement("td", value)));
    const detail = document.createElement("tr");
    detail.className = "queue-expanded-content";
    const cell = document.createElement("td"); cell.colSpan = values.length + 1;
    detail.append(cell); body.append(header, detail);
    const toggle = expanded => {
      if (expanded) open.add(key); else open.delete(key);
      detail.hidden = !expanded;
      header.dataset.open = String(expanded);
      caret.textContent = expanded ? "▾" : "▸";
      caret.setAttribute("aria-expanded", String(expanded));
      caret.setAttribute("aria-label", `${expanded ? "Collapse" : "Expand"} ${values[0]}`);
    };
    toggle(open.has(key));
    header.onclick = () => toggle(detail.hidden);
    return cell;
  }
  statusReason(task) {
    if (!["blocked", "failed"].includes(task.status)) return null;
    const result = task.latest_result || task.accepted_result || {};
    const error = (task.latest_error || "").trim();
    const generic = ["Submitted outcome recorded", "Worker execution ended"].includes(error);
    const detail = (!generic && error) || result.blockers?.join("\n") || result.summary || "No reason was recorded. Inspect the latest conversation and activity.";
    let label = "Task blocker";
    if (/^Paused by operator:/.test(detail)) label = "Paused by you";
    else if (/token budget|token allowance/i.test(detail)) label = "Token limit reached";
    else if (/time budget|execution budget|execution deadline/i.test(detail)) label = "Time limit reached";
    else if (/attempt budget/i.test(detail)) label = "Attempt limit reached";
    else if (/execution infrastructure|structured submission|conversation disappeared/i.test(detail)) label = "Agent execution failed";
    else if (/definition or workspace|required input|workspace could not/i.test(detail)) label = "Inputs or configuration";
    else if (/lease.*expired/i.test(detail)) label = "Lease expired";
    else if (/review round limit/i.test(detail)) label = "Review round limit";
    else if (/without progress/i.test(detail)) label = "Review needs intervention";
    return {label, detail};
  }
  canResume(task) {
    return task.status === "blocked" && !!task.rounds?.length &&
      /^(Review round limit reached|Time budget exhausted:|Token budget exhausted:|Controller (time|token) budget reserved for review:)/.test(task.latest_error || "");
  }
  retryUnavailable(task) {
    return task.attempt_count >= task.max_attempts
      ? `Retry unavailable: ${task.attempt_count}/${task.max_attempts} attempts used. Raising controller token/time limits does not add task attempts.`
      : "";
  }
  workflowStatus(tasks) {
    const statuses = tasks.map(task => task.status);
    if (statuses.length && statuses.every(status => status === "completed"))
      return { state: "completed", label: "Completed" };
    const running = statuses.some(status => ["claimed", "running", "submitted", "reviewing"].includes(status));
    const blocked = statuses.some(status => ["blocked", "failed", "cancelled"].includes(status));
    if (running && blocked)
      return { state: "mixed", label: "Mixed", reason: "Some tasks are running; others are blocked, failed, or cancelled." };
    if (blocked)
      return { state: "blocked", label: "Blocked", reason: "One or more tasks are blocked, failed, or cancelled." };
    if (running)
      return { state: "running", label: "Running" };
    if (statuses.includes("awaiting_review"))
      return { state: "awaiting-human", label: "Awaiting human", reason: "A task needs human approval." };
    return { state: "idle", label: "Idle" };
  }
  expandedRows() {
    const key = this._instance?.instance_id || this._instance?.title;
    if (!this._expandedByController.has(key)) this._expandedByController.set(key, new Set());
    return this._expandedByController.get(key);
  }
  renderTasks(tasks) {
    const rows = this.querySelector(".queue-task-rows");
    const open = this.expandedRows();
    rows.replaceChildren();
    if (!tasks.length) {
      rows.append(textElement("p", "No tasks. Use Load tasks to preview and insert a task batch."));
      return;
    }
    const workflows = new Map();
    for (const task of tasks) {
      if (!workflows.has(task.workflow_id)) workflows.set(task.workflow_id, []);
      workflows.get(task.workflow_id).push(task);
    }
    const table = document.createElement("table"); table.className = "queue-table queue-workflows";
    table.innerHTML = "<thead><tr><th></th><th>Workflow</th><th>Status</th><th>Queued</th><th>Active</th><th>Completed</th><th>Other</th></tr></thead>";
    const body = document.createElement("tbody"); table.append(body); rows.append(table);
    for (const [workflow, members] of workflows) {
      const queued = members.filter(t => t.status === "queued").length;
      const active = members.filter(t => ["claimed", "running", "submitted", "reviewing"].includes(t.status)).length;
      const completed = members.filter(t => t.status === "completed").length;
      const other = members.length - queued - active - completed;
      const status = this.workflowStatus(members);
      const workflowCell = this.expandableRow(body, [workflow, status.label, queued, active, completed, other], `workflow:${workflow}`, open);
      const statusCell = workflowCell.parentElement.previousElementSibling.children[2];
      const badge = textElement("span", status.label, "queue-workflow-status");
      badge.dataset.state = status.state;
      if (status.reason) badge.title = status.reason;
      statusCell.replaceChildren(badge);
      const taskTable = document.createElement("table"); taskTable.className = "queue-table queue-workflow-tasks";
      taskTable.innerHTML = "<thead><tr><th></th><th>Task #</th><th>Name</th><th>Status</th><th>Attempts</th><th>Actions</th></tr></thead>";
      const taskBody = document.createElement("tbody"); taskTable.append(taskBody); workflowCell.append(taskTable);
      for (const task of members) {
      const row = this.expandableRow(taskBody, [`#${task.id}`, task.task_type, task.status === "running" && task.rounds?.length ? `${task.rounds.at(-1).role === "reviewer" ? "Reviewing" : "Working"} · round ${task.rounds.at(-1).number}` : task.status, `${task.attempt_count}/${task.max_attempts}`, ""], `task:${task.id}`, open);
      const header = row.parentElement.previousElementSibling;
      const taskBadge = textElement("span", header.children[3].textContent, "queue-workflow-status");
      taskBadge.dataset.state = this.workflowStatus([task]).state;
      header.children[3].replaceChildren(taskBadge);
      const actionCell = header.children[5];
      actionCell.className = "queue-task-actions";
      actionCell.onclick = event => event.stopPropagation();
      const reason = this.statusReason(task);
      if (reason) {
        const statusCell = row.parentElement.previousElementSibling.children[3];
        const badge = textElement("span", reason.label === "Paused by you" ? "Paused by you" : `${task.status === "blocked" ? "Blocked" : "Failed"} · ${reason.label}`, "queue-workflow-status queue-task-reason-badge");
        badge.dataset.state = "blocked";
        badge.title = reason.detail;
        statusCell.replaceChildren(badge);
        const notice = textElement("div", "", "queue-task-status-reason");
        notice.append(textElement("strong", reason.label), textElement("p", reason.detail));
        const retryReason = this.retryUnavailable(task);
        if (retryReason) notice.append(textElement("p", retryReason));
        row.append(notice);
      }
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
      if (task.rounds?.length) {
        const roundTable = document.createElement("table");
        roundTable.className = "queue-table";
        roundTable.innerHTML = "<thead><tr><th>Round</th><th>Role</th><th>Decision</th><th>Summary</th></tr></thead>";
        const roundBody = document.createElement("tbody"); roundTable.append(roundBody);
        for (const turn of task.rounds) {
          const tr = document.createElement("tr");
          const checkpoint = turn.submission?.origin === "controller_checkpoint";
          const interrupted = !turn.submission && !!turn.submitted_at;
          for (const value of [turn.number, checkpoint ? "Controller checkpoint" : turn.role, checkpoint || interrupted ? "Interrupted" : turn.submission?.decision || turn.submission?.outcome || "In progress", turn.submission?.summary || ""]) {
            tr.append(textElement("td", String(value)));
          }
          roundBody.append(tr);
          if (turn.submission) {
            const evidenceRow = document.createElement("tr"), cell = document.createElement("td"); cell.colSpan = 4;
            const detail = document.createElement("details");
            detail.append(textElement("summary", "Evidence and next steps"), textElement("pre", JSON.stringify(turn.submission, null, 2)));
            cell.append(detail); evidenceRow.append(cell); roundBody.append(evidenceRow);
          }
        }
        row.append(roundTable);
      }
      const actions =
        task.status === "awaiting_review"
          ? ["approve", "reject"]
          : ["failed", "blocked"].includes(task.status)
            ? (this.canResume(task) ? ["resume_attempt", "retry"] : ["retry"])
            : ["completed", "cancelled"].includes(task.status)
              ? []
              : ["cancel"];
      for (const action of actions) {
        const b = textElement("button", action === "resume_attempt" ? "Resume" : action[0].toUpperCase() + action.slice(1));
        const unavailable = action === "retry" ? this.retryUnavailable(task) : "";
        b.disabled = !!unavailable;
        if (unavailable) b.title = unavailable;
        else if (action === "resume_attempt") b.title = "Continue the same attempt and worker conversation under the updated controller limits";
        else if (action === "retry") b.title = "Start a new attempt using the latest task instructions and configuration";
        b.onclick = async event => {
          event.stopPropagation();
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
            b.disabled = !!unavailable;
          }
        };
        actionCell.append(b);
      }
      for (const artifact of (task.latest_result || task.accepted_result)
        ?.artifacts || []) {
        const link = textElement("a", artifact.path);
        link.href = `/api/task-queues/${encodeURIComponent(this._instance.title)}/artifacts/${artifact.sha256}`;
        link.target = "_blank";
        link.rel = "noopener";
        row.append(link);
      }
      }
    }
  }
}
customElements.define("am-task-queue-tasks", AmTaskQueueTasks);
