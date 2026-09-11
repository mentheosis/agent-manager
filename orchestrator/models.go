package orchestration

import "encoding/json"

// Event represents a single event from an agent's history.
type Event struct {
	Input   json.RawMessage `json:"input,omitempty"`
	Message string          `json:"message,omitempty"`
	Seq     int64           `json:"seq,omitempty"`
	Raw     json.RawMessage `json:"-"`
	Type    string          `json:"type"`
	Text    string          `json:"text,omitempty"`
	Output  string          `json:"output,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
	Name    string          `json:"name,omitempty"`
	TS      string          `json:"ts,omitempty"`
}

// HistoryResponse is the response from the /history endpoint.
type HistoryResponse struct {
	Events        []Event `json:"events"`
	TotalCount    int     `json:"total_count"`
	FilteredCount int     `json:"filtered_count"`
	HasMore       bool    `json:"has_more"`
}

// Preserve provider-specific fields while exposing common fields to team formatting.
func (e *Event) UnmarshalJSON(data []byte) error {
	type plain Event
	if err := json.Unmarshal(data, (*plain)(e)); err != nil {
		return err
	}
	e.Raw = append(e.Raw[:0], data...)
	return nil
}
