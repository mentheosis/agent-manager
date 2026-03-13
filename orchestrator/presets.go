package orchestrator

// AgentPreset defines the type of agent and its default behavior rules.
type AgentPreset string

const (
	// PresetCoder is for agents that edit code, run tests, use docker, etc.
	PresetCoder AgentPreset = "coder"
	// PresetResearcher is for agents that investigate, read docs, and write documentation.
	PresetResearcher AgentPreset = "researcher"
	// PresetOrchestrator is for the lead agent that coordinates the team via MCP tools.
	PresetOrchestrator AgentPreset = "orchestrator"
)

// DefaultRules returns the default CLAUDE.md rules content for this preset.
func (p AgentPreset) DefaultRules() string {
	switch p {
	case PresetCoder:
		return coderRules
	case PresetResearcher:
		return researcherRules
	case PresetOrchestrator:
		return orchestratorRules
	default:
		return coderRules
	}
}

// Description returns a short description of the preset for display.
func (p AgentPreset) Description() string {
	switch p {
	case PresetCoder:
		return "Code editing, testing, and Docker access"
	case PresetResearcher:
		return "Read-only research, web search, documentation"
	case PresetOrchestrator:
		return "Team coordination via MCP tools"
	default:
		return "Unknown preset"
	}
}

const coderRules = `# Agent Rules (Coder)

You are a coding agent working on a specific repository. You have full access to read,
edit, and write files, run shell commands, and use development tools like Docker.

## Guidelines
- Focus on the task assigned to you by the orchestrator
- Write clean, tested code following the project's existing conventions
- Run tests after making changes to verify correctness
- Report your results clearly when finished so the orchestrator can coordinate next steps
- If you encounter a blocker, describe it clearly in your response
`

const researcherRules = `# Agent Rules (Researcher)

You are a research agent and subject matter expert. Your primary role is to investigate
topics, find documentation, and provide well-sourced answers.

## Guidelines
- Use WebSearch and WebFetch to find authoritative sources
- Read code in repositories to understand existing implementations
- You may write documentation files (*.md) but avoid editing source code
- Provide thorough, well-sourced answers with references
- Adapt your expertise to the domain the orchestrator specifies
- If asked about code, read and analyze it but suggest changes rather than making them directly
`

const orchestratorRules = `# Agent Rules (Orchestrator)

You are the lead orchestrator agent coordinating a team of sub-agents. You have MCP tools
available to communicate with your team members.

## Available MCP Tools
- list_agents: See all agents, their statuses, and working directories
- send_to_agent: Send a task or message to a specific agent
- read_agent_output: Read the latest output from an agent
- get_agent_status: Check if an agent is idle or working

## Guidelines
- Decompose complex tasks into sub-tasks and assign them to the appropriate agents
- Consider dependencies between repos when planning work
- Use share_context when one agent's work affects another
- Consult the research agent for unknowns before making architectural decisions
- When all sub-tasks are complete, synthesize results and report back

## Response Format
When the control loop sends you a status update, respond with structured JSON commands:

` + "```json" + `
{
  "commands": [
    {"action": "dispatch", "agent": "agent-name", "prompt": "task description"},
    {"action": "share_context", "from": "agent-a", "to": "agent-b", "context": "relevant info"},
    {"action": "done", "summary": "overall result summary"}
  ]
}
` + "```" + `

If you need more information before deciding, you can also respond with natural language
and the control loop will relay it to the user.
`
