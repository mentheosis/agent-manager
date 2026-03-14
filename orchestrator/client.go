package orchestrator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client wraps the claude-squad web server HTTP API.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a new API client pointing at the given base URL.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// InstanceInfo is the subset of instance data the orchestrator needs.
type InstanceInfo struct {
	Title        string `json:"title"`
	DisplayTitle string `json:"display_title,omitempty"`
	Status       string `json:"status"`
	Path         string `json:"path"`
	WorkDir      string `json:"work_dir"`
	Program      string `json:"program"`
	Parent       string `json:"parent,omitempty"`
	AgentPreset  string `json:"agent_preset,omitempty"`
}

// HistoryResponse is the response from the /history endpoint.
type HistoryResponse struct {
	StableLines []string `json:"stable_lines"`
	StableSeqNo int      `json:"stable_seq_no"`
	StableCount int      `json:"stable_count"`
	Pane        []string `json:"pane"`
	LastInput   string   `json:"last_input"`
}

// ListInstances returns all instances from the web server.
func (c *Client) ListInstances() ([]InstanceInfo, error) {
	resp, err := c.httpClient.Get(c.baseURL + "/api/instances")
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list instances returned %d: %s", resp.StatusCode, body)
	}

	var instances []InstanceInfo
	if err := json.NewDecoder(resp.Body).Decode(&instances); err != nil {
		return nil, fmt.Errorf("failed to decode instances: %w", err)
	}
	return instances, nil
}

// GetInstanceStatus returns the status of a single instance.
func (c *Client) GetInstanceStatus(title string) (string, error) {
	resp, err := c.httpClient.Get(c.baseURL + "/api/instances/" + title)
	if err != nil {
		return "", fmt.Errorf("failed to get instance: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get instance returned %d: %s", resp.StatusCode, body)
	}

	var info InstanceInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("failed to decode instance: %w", err)
	}
	return info.Status, nil
}

// SendToInstance sends a prompt to a specific instance.
func (c *Client) SendToInstance(title, text string) error {
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("failed to marshal send body: %w", err)
	}

	resp, err := c.httpClient.Post(
		c.baseURL+"/api/instances/"+title+"/send",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("failed to send to instance %s: %w", title, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send to %s returned %d: %s", title, resp.StatusCode, respBody)
	}
	return nil
}

// GetInstanceHistory returns the conversation history for an instance.
func (c *Client) GetInstanceHistory(title string) (*HistoryResponse, error) {
	resp, err := c.httpClient.Get(c.baseURL + "/api/instances/" + title + "/history")
	if err != nil {
		return nil, fmt.Errorf("failed to get history for %s: %w", title, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get history for %s returned %d: %s", title, resp.StatusCode, body)
	}

	var history HistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&history); err != nil {
		return nil, fmt.Errorf("failed to decode history for %s: %w", title, err)
	}
	return &history, nil
}

// GetInstancePreview returns the current visible pane content for an instance.
func (c *Client) GetInstancePreview(title string) (string, error) {
	resp, err := c.httpClient.Get(c.baseURL + "/api/instances/" + title + "/preview")
	if err != nil {
		return "", fmt.Errorf("failed to get preview for %s: %w", title, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get preview for %s returned %d: %s", title, resp.StatusCode, body)
	}

	var result struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode preview for %s: %w", title, err)
	}
	return result.Content, nil
}
