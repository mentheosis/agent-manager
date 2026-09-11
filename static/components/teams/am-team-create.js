export const DEFAULT_TEAM_YAML = `title: my-team
# Set these paths to your workspace inside the container.
path: .
task: Ask worker-1 to summarize the README and worker-2 to review the summary. Do not edit files.
agents:
  - name: team-leader
    path: .
    preset: orchestrator
    provider: codex
    model: gpt-6-astra
    permission_mode: danger-full-access
  - name: worker-1
    path: .
    preset: coder
    provider: codex
    model: gpt-6-astra
    permission_mode: danger-full-access
  - name: worker-2
    path: .
    preset: coder
    provider: claude
    model: claude-opus-4-8
    permission_mode: bypassPermissions`;

export async function createTeam() {
  const yamlText = this.querySelector("#team-yaml").value.trim();
  const errorDiv = this.querySelector("#yaml-error");
  errorDiv.textContent = "";

  if (!yamlText) {
    errorDiv.textContent = "Please enter YAML configuration";
    return;
  }

  // Parse YAML
  let config;
  try {
    config = this.parseYaml(yamlText);
  } catch (e) {
    errorDiv.textContent = `YAML parse error: ${e.message}`;
    return;
  }

  // Validate
  if (!config.title) {
    errorDiv.textContent = "Missing required field: title";
    return;
  }
  if (!config.path) {
    errorDiv.textContent = "Missing required field: path";
    return;
  }

  if (
    config.agents &&
    (!Array.isArray(config.agents) ||
      config.agents.some((agent) => !agent.name || !agent.path))
  ) {
    errorDiv.textContent = "Each agent needs a name and path";
    return;
  }
  const names = (config.agents || []).map((agent) => agent.name);
  if (new Set(names).size !== names.length) {
    errorDiv.textContent = "Agent names must be unique";
    return;
  }
  const created = [];
  const checkedFetch = async (...args) => {
    const response = await fetch(...args);
    if (!response.ok) {
      const body = await response.json().catch(() => ({}));
      throw new Error(
        body.detail || `Request failed (HTTP ${response.status})`,
      );
    }
    return response;
  };
  const submitBtn = this.querySelector('#team-form button[type="submit"]');
  submitBtn.disabled = true;
  submitBtn.textContent = "Creating...";

  try {
    // Create the loop instance first
    const loopResp = await checkedFetch("/api/instances", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        name: config.title,
        kind: "loop",
        path: config.path,
        permission_mode: "plan",
        model: config.model || null,
        memory_file: config.memory_file || null,
      }),
    });
    if (!loopResp.ok) {
      const err = await loopResp.json();
      throw new Error(err.detail || "Failed to create loop instance");
    }
    const loopInst = await loopResp.json();
    created.push(loopInst.title);

    // Set instance_type to 'loop'
    await checkedFetch(
      `/api/instances/${encodeURIComponent(loopInst.title)}/type`,
      {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ instance_type: "loop" }),
      },
    );

    // Set the task
    if (config.task) {
      await checkedFetch(
        `/api/instances/${encodeURIComponent(loopInst.title)}/task`,
        {
          method: "PATCH",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ task: config.task }),
        },
      );
    }

    // Create child agents
    if (config.agents && Array.isArray(config.agents)) {
      for (const agent of config.agents) {
        if (!agent.name || !agent.path) continue;

        // Create agent instance
        const agentResp = await checkedFetch("/api/instances", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            name: agent.name,
            path: agent.path,
            provider: agent.provider || config.provider || "claude",
            permission_mode:
              agent.permission_mode ||
              ((agent.provider || config.provider) === "codex"
                ? "workspace-write"
                : "acceptEdits"),
            model: agent.model || null,
            memory_file: agent.memory_file || null,
          }),
        });
        if (!agentResp.ok) {
          console.error(`Failed to create agent ${agent.name}`);
          continue;
        }
        const agentInst = await agentResp.json();
        created.push(agentInst.title);

        // Reparent to the loop instance
        await checkedFetch(
          `/api/instances/${encodeURIComponent(agentInst.title)}/reparent`,
          {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ parent: loopInst.title }),
          },
        );

        // Set agent_preset if specified
        if (agent.preset) {
          await checkedFetch(
            `/api/instances/${encodeURIComponent(agentInst.title)}/type`,
            {
              method: "PATCH",
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify({ agent_preset: agent.preset }),
            },
          );
        }
      }
    }

    // Dispatch event and close
    this.dispatchEvent(
      new CustomEvent("instance-created", {
        bubbles: true,
        detail: { title: loopInst.title },
      }),
    );
    this.close();
  } catch (e) {
    errorDiv.textContent =
      e.message +
      (created.length ? ` Created so far: ${created.join(", ")}.` : "");
  } finally {
    submitBtn.disabled = false;
    submitBtn.textContent = "Create Team";
  }
}
