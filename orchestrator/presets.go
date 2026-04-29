package main

import _ "embed"

//go:embed presets/coder.md
var coderRules string

//go:embed presets/researcher.md
var researcherRules string

//go:embed presets/orchestrator.md
var orchestratorRules string

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
