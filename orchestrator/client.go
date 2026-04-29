package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client wraps the agent-manager web server HTTP API.
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
	Title        string   `json:"title"`
	DisplayTitle string   `json:"display_title,omitempty"`
	Status       string   `json:"status"`
	Path         string   `json:"path"`
	Parent       string   `json:"parent,omitempty"`
	Children     []string `json:"children,omitempty"`
	AgentPreset  string   `json:"agent_preset,omitempty"`
	InstanceType string   `json:"instance_type,omitempty"`
	Task         string   `json:"task,omitempty"`
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

// GetInstance returns info for a single instance.
func (c *Client) GetInstance(title string) (*InstanceInfo, error) {
	resp, err := c.httpClient.Get(c.baseURL + "/api/instances/" + url.PathEscape(title))
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

	resp, err := c.httpClient.Post(
		c.baseURL+"/api/instances/"+url.PathEscape(title)+"/send",
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
		c.baseURL, url.PathEscape(title), tail, offset)
	if types != "" {
		u += "&types=" + url.QueryEscape(types)
	}

	resp, err := c.httpClient.Get(u)
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
	resp, err := c.httpClient.Get(c.baseURL + "/api/instances/" + url.PathEscape(title) + "/children")
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
