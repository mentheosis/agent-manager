package orchestrator

import _ "embed"

//go:embed prompts/coder.txt
var coderRules string

//go:embed prompts/researcher.txt
var researcherRules string

//go:embed prompts/orchestrator.txt
var orchestratorRules string

//go:embed prompts/settings_coder.json
var coderSettings string

//go:embed prompts/settings_researcher.json
var researcherSettings string

//go:embed prompts/settings_orchestrator.json
var orchestratorSettings string

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

// DefaultSettings returns the default settings.local.json content for this preset.
func (p AgentPreset) DefaultSettings() string {
	switch p {
	case PresetCoder:
		return coderSettings
	case PresetResearcher:
		return researcherSettings
	case PresetOrchestrator:
		return orchestratorSettings
	default:
		return coderSettings
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
