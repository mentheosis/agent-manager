package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client wraps the agent-manager web server HTTP API.
type HTTPStatusError struct{ StatusCode int }

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("agent API returned HTTP %d", e.StatusCode)
}

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewClient creates a new API client pointing at the given base URL.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// InstanceInfo is the subset of instance data the orchestrator needs.
type InstanceInfo struct {
	Title        string   `json:"title"`
	DisplayTitle string   `json:"display_title,omitempty"`
	Status       string   `json:"status"`
	Path         string   `json:"path"`
	Provider     string   `json:"provider,omitempty"`
	Kind         string   `json:"kind,omitempty"`
	Parent       string   `json:"parent,omitempty"`
	Children     []string `json:"children,omitempty"`
	AgentPreset  string   `json:"agent_preset,omitempty"`
	InstanceType string   `json:"instance_type,omitempty"`
	Task         string   `json:"task,omitempty"`
}

// ListInstances returns all instances from the web server.
func (c *Client) ListInstances() ([]InstanceInfo, error) {
	resp, err := c.HTTPClient.Get(c.BaseURL + "/api/instances")
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

// GetInstance returns info for a single instance.
func (c *Client) GetInstance(title string) (*InstanceInfo, error) {
	resp, err := c.HTTPClient.Get(c.BaseURL + "/api/instances/" + url.PathEscape(title))
	if err != nil {
		return nil, fmt.Errorf("failed to get instance: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get instance returned %d: %s", resp.StatusCode, body)
	}

	var info InstanceInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("failed to decode instance: %w", err)
	}
	return &info, nil
}

// GetInstanceStatus returns the status of a single instance.
func (c *Client) GetInstanceStatus(title string) (string, error) {
	info, err := c.GetInstance(title)
	if err != nil {
		return "", err
	}
	return info.Status, nil
}

// SendToInstance sends a prompt to a specific instance.
func (c *Client) SendToInstance(title, text string) error {
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("failed to marshal send body: %w", err)
	}

	resp, err := c.HTTPClient.Post(
		c.BaseURL+"/api/instances/"+url.PathEscape(title)+"/send",
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

// GetInstanceHistory returns the event history for an instance with pagination.
func (c *Client) GetInstanceHistory(title string, tail, offset int, types string) (*HistoryResponse, error) {
	u := fmt.Sprintf("%s/api/instances/%s/history?tail=%d&offset=%d",
		c.BaseURL, url.PathEscape(title), tail, offset)
	if types != "" {
		u += "&types=" + url.QueryEscape(types)
	}

	resp, err := c.HTTPClient.Get(u)
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

// GetChildren returns the child instances of a loop instance.
func (c *Client) GetChildren(title string) ([]InstanceInfo, error) {
	resp, err := c.HTTPClient.Get(c.BaseURL + "/api/instances/" + url.PathEscape(title) + "/children")
	if err != nil {
		return nil, fmt.Errorf("failed to get children for %s: %w", title, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get children for %s returned %d: %s", title, resp.StatusCode, body)
	}

	var children []InstanceInfo
	if err := json.NewDecoder(resp.Body).Decode(&children); err != nil {
		return nil, fmt.Errorf("failed to decode children for %s: %w", title, err)
	}
	return children, nil
}

func (c *Client) Request(ctx context.Context, method, path string, input, output any, token string) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPStatusError{StatusCode: resp.StatusCode}
	}
	if output != nil {
		return json.NewDecoder(resp.Body).Decode(output)
	}
	return nil
}
