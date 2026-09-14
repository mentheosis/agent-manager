import { queueRequest } from "./api.js";
class AmTaskQueueLoader extends HTMLElement {
  connectedCallback() {
    this.innerHTML = `<dialog><form method="dialog"><button class="close">Close</button></form><h3>Load tasks</h3><p>Paste a batch JSON or choose a file. Preview validates and renders each task. Loading inserts tasks; an active controller may start eligible tasks immediately.</p><input type="file" accept="application/json,.json" aria-label="Task batch file"><textarea rows="14" wrap="off" spellcheck="false" aria-label="Task batch JSON"></textarea><div><button class="preview">Preview tasks</button><button class="enqueue" disabled>Load into queue</button></div><p class="loader-message" role="status"></p><select class="preview-tasks" aria-label="Preview task" hidden></select><pre class="rendered"></pre><button class="done" hidden>Close</button></dialog>`;
    this.querySelector(".done").onclick = () => this.querySelector("dialog").close();
    this.querySelector(".preview-tasks").onchange = e => this.showTask(Number(e.target.value));
    this.querySelector("input").onchange = async (e) => {
      const file = e.target.files[0];
      if (!file) return;
      if (file.size > 1048576) { this.message("Batch exceeds 1 MiB"); return; }
      this.querySelector("textarea").value = await file.text(); this.invalidate();
    };
    this.querySelector("textarea").oninput = () => this.invalidate();
    this.querySelector(".preview").onclick = () => this.run("render");
    this.querySelector(".enqueue").onclick = () => this.run("enqueue");
  }
  message(value) { this.querySelector(".loader-message").textContent = value; }
  invalidate() { this._preview = null; this.querySelector(".done").hidden = true; this.querySelector(".preview-tasks").hidden = true; this.querySelector(".rendered").textContent = ""; this._revision = (this._revision || 0) + 1; this.querySelector(".enqueue").disabled = true; }
  open(instance) {
    this._instance = instance; this.invalidate();
    this.querySelector("textarea").value = JSON.stringify({ batch_key: crypto.randomUUID(), workflow_id: "first-run", tasks: [{ key: "first", task_type: "<task-type>", parameters: {}, human_review_required: true }] }, null, 2);
    this.querySelector(".rendered").textContent = ""; this.message("");
    this.querySelector("dialog").showModal();
  }
  showTask(index) {
    const t = this._renderedTasks?.[index];
    this.querySelector(".rendered").textContent = t ? `TASK ${t.key} (${t.task_type})\n${t.execution.provider} · ${t.execution.model || "default model"} · ${t.execution.permission}\nWorkspace: ${t.working_directory} (${t.use_isolated_workspace ? "isolated" : "shared"})\n\n${t.prompt}\n\n${(t.warnings || []).join("\n")}` : "";
  }
  async run(action) {
    const revision = this._revision, title = this._instance?.title;
    const buttons = [this.querySelector(".preview"), this.querySelector(".enqueue")];
    buttons.forEach(b => b.disabled = true);
    try {
      const batch = JSON.parse(this.querySelector("textarea").value);
      if (action === "enqueue") {
        if (!this._preview) throw new Error("Preview the batch first");
        batch.preview_hash = this._preview.preview_hash;
      }
      const result = await queueRequest(title, action, batch);
      if (revision !== this._revision || title !== this._instance?.title) return;
      if (action === "render") {
        this._preview = result.error ? null : result;
        this._renderedTasks = result.tasks || [];
        const select = this.querySelector(".preview-tasks");
        select.replaceChildren();
        this._renderedTasks.forEach((task, index) => {
          const option = document.createElement("option");
          option.value = index; option.textContent = `${task.key} · ${task.task_type}`;
          select.append(option);
        });
        select.hidden = !this._renderedTasks.length;
        this.showTask(0);
        const count = batch.tasks.length, passed = this._renderedTasks.length;
        this.message(result.error
          ? `${count} tasks parsed · ${passed} rendered · validation stopped at an error; ${count - passed} tasks unresolved. ${result.error}`
          : `${count} tasks parsed · ${passed} rendered · 0 failed · ${this._renderedTasks.filter(t => t.warnings?.length).length} with notes. Select a task to inspect it.`);
      } else {
        this.invalidate();
        this.message(`${result.task_ids.length} tasks loaded successfully. Task IDs: ${result.task_ids.join(", ")}. Repeated batches reuse existing tasks.`);
        this.querySelector(".done").hidden = false;
        document.dispatchEvent(new CustomEvent("queue-updated", { detail: { title } }));
      }
    } catch (error) { this.message(error.message); if (action === "render") this._preview = null; }
    finally { this.querySelector(".preview").disabled = false; this.querySelector(".enqueue").disabled = !this._preview; }
  }
}
customElements.define("am-task-queue-loader", AmTaskQueueLoader);
