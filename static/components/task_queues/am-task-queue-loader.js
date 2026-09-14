import { queueRequest } from "./api.js";
class AmTaskQueueLoader extends HTMLElement {
  connectedCallback() {
    this.innerHTML = `<dialog><form method="dialog"><button class="close">Close</button></form><h3>Load tasks</h3><p>Paste a batch JSON or choose a file. Preview validates and renders each task. Loading inserts tasks; an active controller may start eligible tasks immediately.</p><input type="file" accept="application/json,.json" aria-label="Task batch file"><textarea rows="14" wrap="off" spellcheck="false" aria-label="Task batch JSON"></textarea><div><button class="preview">Preview tasks</button><button class="enqueue" disabled>Load into queue</button></div><p class="loader-message" role="status"></p><pre class="rendered"></pre></dialog>`;
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
  invalidate() { this._preview = null; this._revision = (this._revision || 0) + 1; this.querySelector(".enqueue").disabled = true; }
  open(instance) {
    this._instance = instance; this.invalidate();
    this.querySelector("textarea").value = JSON.stringify({ batch_key: crypto.randomUUID(), workflow_id: "first-run", tasks: [{ key: "first", task_type: "<task-type>", parameters: {}, human_review_required: true }] }, null, 2);
    this.querySelector(".rendered").textContent = ""; this.message("");
    this.querySelector("dialog").showModal();
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
        this._preview = result;
        this.querySelector(".rendered").textContent = result.tasks.map(t => `TASK ${t.key} (${t.task_type})\n${t.execution.provider} · ${t.execution.model || "default model"} · ${t.execution.permission}\nWorkspace: ${t.working_directory || "configured repository"}${t.use_isolated_workspace ? " (isolated export at execution)" : " (shared)"}\n\n${t.prompt}\n${t.warnings.length ? "\nNOTES\n" + t.warnings.join("\n") : ""}`).join("\n\n────────\n\n");
        this.message("Preview ready. Missing or pending inputs are checked again before execution.");
      } else {
        this.message("Loaded task IDs: " + JSON.stringify(result.task_ids));
        this.invalidate();
        document.dispatchEvent(new CustomEvent("queue-updated", { detail: { title } }));
      }
    } catch (error) { this.message(error.message); if (action === "render") this._preview = null; }
    finally { this.querySelector(".preview").disabled = false; this.querySelector(".enqueue").disabled = !this._preview; }
  }
}
customElements.define("am-task-queue-loader", AmTaskQueueLoader);
