class AmTaskQueueCreate extends HTMLElement {
  connectedCallback() {
    this.innerHTML = `<form><label>Name<input name="name" value="Task queue" required></label><label>Queue profile<select name="profile" required></select></label><label>Initial max workers<input name="max" type="number" value="1" min="1" required></label><label>Lease seconds<input name="lease" type="number" min="15" value="60" required></label><label>Task time limit (seconds)<input name="seconds" type="number" min="15" value="3600" required></label><label>Task token limit<input name="tokens" type="number" min="1" value="2000000" required></label><p>Profiles are configured on the server. Database credentials stay out of the browser. The profile name is the queue identifier used by task rows. Each controller has its own worker limit and pause control.</p><p class="queue-error" role="alert"></p><button type="submit">Create queue</button></form>`;
    this.querySelector("form").addEventListener("submit", (event) => {
      event.preventDefault();
      this.create();
    });
  }
  async loadProfiles() {
    try {
      const response = await fetch("/api/task-queue-profiles");
      if (!response.ok) throw new Error("Could not load queue profiles");
      const profiles = await response.json();
      const select = this.querySelector("select");
      select.replaceChildren();
      for (const profile of profiles) {
        const option = document.createElement("option");
        option.value = profile.name;
        option.textContent = profile.name;
        select.append(option);
      }
      const applyDefaults = () => {
        const profile = profiles.find(p => p.name === select.value);
        for (const [field, key, fallback] of [["max", "default_max_workers", 1], ["lease", "default_lease_secs", 60], ["seconds", "default_task_limit_secs", 3600], ["tokens", "default_task_limit_tokens", 2000000]])
          this.querySelector(`[name=${field}]`).value = profile?.[key] ?? fallback;
      };
      select.onchange = applyDefaults;
      applyDefaults();
      this.querySelector(".queue-error").textContent = profiles.length
        ? ""
        : "Configure AM_TASK_QUEUE_PROFILES on the server to add a queue.";
    } catch (error) {
      this.querySelector(".queue-error").textContent = error.message;
    }
  }
  async create() {
    const button = this.querySelector("button");
    button.disabled = true;
    try {
      const response = await fetch("/api/instances", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          name: this.querySelector("[name=name]").value,
          path: ".",
          kind: "loop",
          controller_mode: "task_queue",
          queue_profile: this.querySelector("select").value,
          queue_id: this.querySelector("select").value,
          queue_lease_secs: Number(this.querySelector("[name=lease]").value),
          queue_task_limit_secs: Number(this.querySelector("[name=seconds]").value),
          queue_task_limit_tokens: Number(this.querySelector("[name=tokens]").value),
          queue_initial_max_workers: Number(
            this.querySelector("[name=max]").value,
          ),
        }),
      });
      const data = await response.json();
      if (!response.ok)
        throw new Error(data.detail || "Could not create queue");
      this.dispatchEvent(
        new CustomEvent("instance-created", {
          bubbles: true,
          detail: { title: data.title },
        }),
      );
      this.closest("am-new-dialog").close();
    } catch (error) {
      this.querySelector(".queue-error").textContent = error.message;
    } finally {
      button.disabled = false;
    }
  }
}
customElements.define("am-task-queue-create", AmTaskQueueCreate);
