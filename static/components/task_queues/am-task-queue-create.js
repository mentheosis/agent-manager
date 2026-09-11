class AmTaskQueueCreate extends HTMLElement {
  connectedCallback() {
    this.innerHTML = `<form><label>Name<input name="name" value="Task queue" required></label><label>Queue profile<select name="profile" required></select></label><label>Queue identifier<input name="queue-id" placeholder="task-pool-id" pattern="[A-Za-z0-9_-]{1,128}" required></label><label>Initial max workers<input name="max" type="number" value="1" min="1" required></label><p>Profiles are configured on the server. Database credentials stay out of the browser. The identifier matches task rows. Each controller has its own worker limit and pause control.</p><p class="queue-error" role="alert"></p><button type="submit">Create queue</button></form>`;
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
      const identifier = this.querySelector('[name=queue-id]');
      if (!identifier.value && profiles[0]?.queue_id) identifier.value = profiles[0].queue_id;
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
          queue_id: this.querySelector("[name=queue-id]").value,
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
