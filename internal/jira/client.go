package jira

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client wraps the Jira REST API v2.
type Client struct {
	baseURL string
	pat     string
	http    *http.Client
}

func NewClient(baseURL, pat string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		pat:     pat,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Status represents a Jira workflow status.
type Status struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ProjectStatuses returns all unique statuses available in a project,
// deduplicated across issue types.
func (c *Client) ProjectStatuses(projectKey string) ([]Status, error) {
	url := fmt.Sprintf("%s/rest/api/2/project/%s/statuses", c.baseURL, projectKey)
	body, err := c.get(url)
	if err != nil {
		return nil, err
	}

	// Response is an array of issue types, each with a statuses array.
	var issueTypes []struct {
		Statuses []Status `json:"statuses"`
	}
	if err := json.Unmarshal(body, &issueTypes); err != nil {
		return nil, fmt.Errorf("parse project statuses: %w", err)
	}

	seen := map[string]bool{}
	var result []Status
	for _, it := range issueTypes {
		for _, s := range it.Statuses {
			if !seen[s.Name] {
				seen[s.Name] = true
				result = append(result, s)
			}
		}
	}
	return result, nil
}

// AssignToSelf assigns the issue to the authenticated user (currentUser).
func (c *Client) AssignToSelf(issueKey string) error {
	url := fmt.Sprintf("%s/rest/api/2/issue/%s/assignee", c.baseURL, issueKey)
	// Setting name to "-1" assigns to the current user in Jira Server/DC.
	// For Jira Cloud, we need accountId. We'll try the myself endpoint first.
	myself, err := c.currentUser()
	if err != nil {
		return fmt.Errorf("get current user: %w", err)
	}

	payload := map[string]string{}
	if myself.AccountID != "" {
		// Jira Cloud
		payload["accountId"] = myself.AccountID
	} else {
		// Jira Server/DC
		payload["name"] = myself.Name
	}

	return c.put(url, payload)
}

// Unassign removes the assignee from an issue.
func (c *Client) Unassign(issueKey string) error {
	url := fmt.Sprintf("%s/rest/api/2/issue/%s/assignee", c.baseURL, issueKey)
	// Detect Cloud vs Server the same way AssignToSelf does.
	myself, err := c.currentUser()
	if err != nil {
		return fmt.Errorf("get current user: %w", err)
	}
	if myself.AccountID != "" {
		// Jira Cloud: null accountId clears assignee
		return c.put(url, map[string]*string{"accountId": nil})
	}
	// Jira Server/DC: empty name clears assignee
	return c.put(url, map[string]string{"name": ""})
}

// TransitionTo transitions an issue to the target status name.
// It finds the appropriate transition by matching the target status name.
func (c *Client) TransitionTo(issueKey, targetStatusName string) error {
	transitions, err := c.getTransitions(issueKey)
	if err != nil {
		return err
	}

	for _, t := range transitions {
		if strings.EqualFold(t.To.Name, targetStatusName) {
			return c.doTransition(issueKey, t.ID)
		}
	}

	available := make([]string, len(transitions))
	for i, t := range transitions {
		available[i] = t.To.Name
	}
	return fmt.Errorf("no transition to %q found (available: %s)", targetStatusName, strings.Join(available, ", "))
}

// Issue represents core fields of a Jira issue.
type Issue struct {
	Key    string `json:"key"`
	Self   string `json:"self"`
	Fields struct {
		Summary     string  `json:"summary"`
		Description string  `json:"description"`
		Status      *Status `json:"status,omitempty"`
		IssueType   *struct {
			Name string `json:"name"`
		} `json:"issuetype,omitempty"`
		Priority *struct {
			Name string `json:"name"`
		} `json:"priority,omitempty"`
		Assignee *struct {
			DisplayName string `json:"displayName"`
			AccountID   string `json:"accountId"`
			Name        string `json:"name"`
		} `json:"assignee,omitempty"`
		Parent *struct {
			Key string `json:"key"`
		} `json:"parent,omitempty"`
		Labels []string `json:"labels,omitempty"`
	} `json:"fields"`
}

// IssueType represents a Jira issue type for a project.
type IssueType struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Subtask  bool   `json:"subtask"`
	IconURL  string `json:"iconUrl,omitempty"`
}

// Transition represents an available workflow transition.
type Transition struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	To   Status `json:"to"`
}

// GetIssue fetches a single issue by key.
func (c *Client) GetIssue(issueKey string) (*Issue, error) {
	url := fmt.Sprintf("%s/rest/api/2/issue/%s", c.baseURL, issueKey)
	body, err := c.get(url)
	if err != nil {
		return nil, err
	}
	var issue Issue
	if err := json.Unmarshal(body, &issue); err != nil {
		return nil, fmt.Errorf("parse issue: %w", err)
	}
	return &issue, nil
}

// AddComment posts a comment on an issue.
func (c *Client) AddComment(issueKey, body string) error {
	url := fmt.Sprintf("%s/rest/api/2/issue/%s/comment", c.baseURL, issueKey)
	return c.post(url, map[string]string{"body": body})
}

// GetTransitions returns the available workflow transitions for an issue.
func (c *Client) GetTransitions(issueKey string) ([]Transition, error) {
	return c.getTransitions(issueKey)
}

// ListIssueTypes returns the issue types available in a project.
func (c *Client) ListIssueTypes(projectKey string) ([]IssueType, error) {
	url := fmt.Sprintf("%s/rest/api/2/project/%s", c.baseURL, projectKey)
	body, err := c.get(url)
	if err != nil {
		return nil, err
	}
	var project struct {
		IssueTypes []IssueType `json:"issueTypes"`
	}
	if err := json.Unmarshal(body, &project); err != nil {
		return nil, fmt.Errorf("parse project: %w", err)
	}
	return project.IssueTypes, nil
}

// CreateIssue creates a new issue. If parentKey is non-empty, the issue is
// linked as a child (Cloud uses fields.parent, Server/DC uses Epic Link).
func (c *Client) CreateIssue(projectKey, issueType, summary, description, parentKey string) (string, error) {
	fields := map[string]any{
		"project":   map[string]string{"key": projectKey},
		"issuetype": map[string]string{"name": issueType},
		"summary":   summary,
	}
	if description != "" {
		fields["description"] = description
	}

	if parentKey != "" {
		myself, err := c.currentUser()
		if err != nil {
			return "", fmt.Errorf("detect cloud/server: %w", err)
		}
		if myself.AccountID != "" {
			// Jira Cloud: native parent field
			fields["parent"] = map[string]string{"key": parentKey}
		} else {
			// Jira Server/DC: discover Epic Link custom field
			epicField, err := c.epicLinkField()
			if err != nil {
				return "", fmt.Errorf("discover epic link field: %w", err)
			}
			if epicField != "" {
				fields[epicField] = parentKey
			}
		}
	}

	payload := map[string]any{"fields": fields}
	respBody, err := c.postJSON(fmt.Sprintf("%s/rest/api/2/issue", c.baseURL), payload)
	if err != nil {
		return "", err
	}

	var result struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse create response: %w", err)
	}
	return result.Key, nil
}

// SetParent links an existing issue under a parent.
// Cloud: updates fields.parent. Server/DC: updates the Epic Link custom field.
func (c *Client) SetParent(issueKey, parentKey string) error {
	myself, err := c.currentUser()
	if err != nil {
		return fmt.Errorf("detect cloud/server: %w", err)
	}

	fields := map[string]any{}
	if myself.AccountID != "" {
		fields["parent"] = map[string]string{"key": parentKey}
	} else {
		epicField, err := c.epicLinkField()
		if err != nil {
			return fmt.Errorf("discover epic link field: %w", err)
		}
		if epicField == "" {
			return fmt.Errorf("could not find Epic Link custom field on this Jira instance")
		}
		fields[epicField] = parentKey
	}

	url := fmt.Sprintf("%s/rest/api/2/issue/%s", c.baseURL, issueKey)
	return c.put(url, map[string]any{"fields": fields})
}

// epicLinkField discovers the custom field ID for Epic Link on Server/DC.
// It looks for the field with schema type "com.pyxis.greenhopper.jira:gh-epic-link".
// Returns empty string (not an error) if not found.
func (c *Client) epicLinkField() (string, error) {
	body, err := c.get(fmt.Sprintf("%s/rest/api/2/field", c.baseURL))
	if err != nil {
		return "", err
	}

	var fields []struct {
		ID     string `json:"id"`
		Schema struct {
			Custom string `json:"custom"`
		} `json:"schema"`
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", fmt.Errorf("parse fields: %w", err)
	}

	for _, f := range fields {
		if f.Schema.Custom == "com.pyxis.greenhopper.jira:gh-epic-link" {
			return f.ID, nil
		}
	}
	return "", nil
}

// --- internal helpers ---

type currentUserResponse struct {
	Name      string `json:"name"`      // Jira Server/DC
	AccountID string `json:"accountId"` // Jira Cloud
}

func (c *Client) currentUser() (*currentUserResponse, error) {
	body, err := c.get(fmt.Sprintf("%s/rest/api/2/myself", c.baseURL))
	if err != nil {
		return nil, err
	}
	var user currentUserResponse
	if err := json.Unmarshal(body, &user); err != nil {
		return nil, fmt.Errorf("parse myself: %w", err)
	}
	return &user, nil
}

func (c *Client) getTransitions(issueKey string) ([]Transition, error) {
	url := fmt.Sprintf("%s/rest/api/2/issue/%s/transitions", c.baseURL, issueKey)
	body, err := c.get(url)
	if err != nil {
		return nil, err
	}

	var result struct {
		Transitions []Transition `json:"transitions"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse transitions: %w", err)
	}
	return result.Transitions, nil
}

func (c *Client) doTransition(issueKey, transitionID string) error {
	url := fmt.Sprintf("%s/rest/api/2/issue/%s/transitions", c.baseURL, issueKey)
	payload := map[string]any{
		"transition": map[string]string{"id": transitionID},
	}
	return c.post(url, payload)
}

func (c *Client) get(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d: %s", url, resp.StatusCode, string(body))
	}
	return body, nil
}

func (c *Client) put(url string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("PUT", url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PUT %s returned %d: %s", url, resp.StatusCode, string(body))
	}
	return nil
}

func (c *Client) postJSON(url string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("POST %s returned %d: %s", url, resp.StatusCode, string(body))
	}
	return body, nil
}

func (c *Client) post(url string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s returned %d: %s", url, resp.StatusCode, string(body))
	}
	return nil
}
