package taskqueues

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Handoff is user-facing context, independent of any repository or task domain.
type Handoff struct {
	Headline   string `json:"headline"`
	Completed  string `json:"completed"`
	Remaining  string `json:"remaining"`
	Reason     string `json:"reason"`
	NextAction string `json:"next_action"`
	Actor      string `json:"actor"`
	Category   string `json:"category"`
	Readiness  string `json:"readiness"`
	RetryAfter string `json:"retry_after,omitempty"`
}

func (h *Handoff) validate() error {
	// Old in-flight clients remain compatible; new tool schemas require this field.
	if h == nil {
		return nil
	}
	for _, value := range []string{h.Headline, h.Completed, h.Remaining, h.Reason, h.NextAction, h.Actor, h.Readiness} {
		if strings.TrimSpace(value) == "" || len(value) > 4000 {
			return errors.New("handoff fields must be nonempty plain-language text, at most 4000 characters each")
		}
	}
	switch h.Category {
	case "none", "human_input", "waiting", "engineering", "system_error", "limit":
	default:
		return errors.New("invalid handoff category")
	}
	if h.RetryAfter != "" {
		if _, err := time.Parse(time.RFC3339, h.RetryAfter); err != nil {
			return errors.New("handoff retry_after must be RFC3339")
		}
	}
	return nil
}

type TaskExplanation struct {
	Handoff
	Source    string     `json:"source"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

func explainTask(t Task) TaskExplanation {
	x := TaskExplanation{Handoff: Handoff{Headline: t.Status, Category: "none"}, Source: "controller", UpdatedAt: t.UpdatedAt}
	var p Submission
	_ = json.Unmarshal(t.LatestResult, &p)
	var timestamp *time.Time
	source := "worker"
	if p.HandoffSource == "reviewer" {
		source = "reviewer"
	}
	var latest *Round
	if len(t.Rounds) > 0 {
		latest = &t.Rounds[len(t.Rounds)-1]
		timestamp = latest.SubmittedAt
		if latest.Role == "reviewer" && len(latest.Submission) > 0 {
			var d ReviewDecision
			_ = json.Unmarshal(latest.Submission, &d)
			p.Summary = d.Summary
			p.Handoff = d.Handoff
			p.Blockers = []string{d.ExternalDependency}
			source = "reviewer"
			if p.Handoff == nil {
				p.Handoff = &Handoff{Headline: d.Summary, Reason: d.ExternalDependency, NextAction: strings.Join(d.NextSteps, "\n"), Category: "human_input"}
				if d.Decision == "continue" {
					p.Handoff.Category = "engineering"
				}
			}
		} else if latest.Role == "worker" {
			_ = json.Unmarshal(latest.Submission, &p)
		}
	}
	if p.Handoff != nil {
		x.Handoff = *p.Handoff
	} else if p.Summary != "" {
		x.Headline = p.Summary
		x.Reason = strings.Join(p.Blockers, "\n")
	}
	x.Source = source
	x.UpdatedAt = timestamp
	if x.UpdatedAt == nil {
		x.UpdatedAt = t.UpdatedAt
	}
	// Running state wins over previous verdicts. Never show an old blocker as current.
	if t.Status == "running" || t.Status == "claimed" || t.Status == "submitted" || t.Status == "reviewing" {
		x = TaskExplanation{Handoff: Handoff{Headline: "Worker is working", Category: "none", NextAction: "No action needed; work is in progress."}, Source: "controller", UpdatedAt: t.UpdatedAt}
		if t.Status == "reviewing" || t.Status == "submitted" || (latest != nil && latest.Role == "reviewer") {
			x.Headline = "Awaiting review"
			x.NextAction = "The reviewer will assess the worker's submission."
		}
		return x
	}
	if t.Status == "queued" || t.Status == "retry_wait" {
		return TaskExplanation{Handoff: Handoff{Headline: "Waiting to start", Category: "none"}, Source: "controller", UpdatedAt: t.UpdatedAt}
	}
	if t.Status == "completed" {
		x.Headline = "Completed"
		x.Category = "none"
	}
	if t.Status == "awaiting_review" {
		x.Headline = "Awaiting human approval"
		x.Category = "human_input"
		x.Actor = "human"
		x.NextAction = "Review the accepted proposal, then approve or reject this task."
	}
	reason := strings.TrimSpace(t.LatestError)
	if (t.Status == "blocked" || t.Status == "failed" || t.Status == "cancelled") && reason != "" && reason != p.Summary && reason != "Submitted outcome recorded" && reason != "Worker execution ended" {
		x = TaskExplanation{Handoff: Handoff{Headline: "Controller stopped the task", Category: "system_error", Reason: reason, Actor: "human", NextAction: "Resolve the controller error, then resume or retry the task."}, Source: "controller", UpdatedAt: t.UpdatedAt}
		lower := strings.ToLower(reason)
		if strings.Contains(lower, "budget") || strings.Contains(lower, "round limit") {
			x.Headline = "Controller limit reached"
			x.Category = "limit"
			x.NextAction = "Increase the indicated controller limit, then resume the same attempt."
		}
		if strings.HasPrefix(reason, "Paused by operator:") {
			x.Headline = "Paused by you"
			x.Category = "human_input"
			x.NextAction = "Resume when ready."
		}
	}
	return x
}
