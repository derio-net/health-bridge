package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

var githubGraphQLURL = "https://api.github.com/graphql"
var githubRESTURL = "https://api.github.com"

// setGitHubURLs overrides the API base URLs (used by tests).
func setGitHubURLs(graphQL, rest string) {
	githubGraphQLURL = graphQL
	githubRESTURL = rest
}

// GitHubClient handles GitHub API interactions for project lifecycle management.
type GitHubClient struct {
	token         string
	org           string
	projectNumber int
	projectID     string
	fieldID       string
	optionIDs     map[string]string // lifecycle state name → option ID
	httpClient    *http.Client
}

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// NewGitHubClient creates a client and loads project metadata (project ID, field ID, option IDs).
func NewGitHubClient(token, org string, projectNumber int) (*GitHubClient, error) {
	c := &GitHubClient{
		token:         token,
		org:           org,
		projectNumber: projectNumber,
		optionIDs:     make(map[string]string),
		httpClient:    &http.Client{Timeout: 30 * time.Second},
	}
	if err := c.loadProjectMetadata(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *GitHubClient) loadProjectMetadata() error {
	query := `query($org: String!, $number: Int!) {
		organization(login: $org) {
			projectV2(number: $number) {
				id
				field(name: "Lifecycle") {
					... on ProjectV2SingleSelectField {
						id
						options {
							id
							name
						}
					}
				}
			}
		}
	}`

	vars := map[string]any{
		"org":    c.org,
		"number": c.projectNumber,
	}

	resp, err := c.graphQL(query, vars)
	if err != nil {
		return fmt.Errorf("load project metadata: %w", err)
	}

	var result struct {
		Organization struct {
			ProjectV2 struct {
				ID    string `json:"id"`
				Field struct {
					ID      string `json:"id"`
					Options []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"options"`
				} `json:"field"`
			} `json:"projectV2"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("parse project metadata: %w", err)
	}

	project := result.Organization.ProjectV2
	if project.ID == "" {
		return fmt.Errorf("project #%d not found in org %s", c.projectNumber, c.org)
	}
	c.projectID = project.ID

	if project.Field.ID == "" {
		return fmt.Errorf("'Lifecycle' field not found on project #%d", c.projectNumber)
	}
	c.fieldID = project.Field.ID

	for _, opt := range project.Field.Options {
		c.optionIDs[opt.Name] = opt.ID
	}

	log.Printf("Loaded project metadata: id=%s, field=%s, %d lifecycle states",
		c.projectID, c.fieldID, len(c.optionIDs))
	return nil
}

// UpdateLifecycleState finds the Issue's project item and updates its Lifecycle field.
func (c *GitHubClient) UpdateLifecycleState(repo string, issueNumber int, newState string) error {
	optionID, ok := c.optionIDs[newState]
	if !ok {
		return fmt.Errorf("unknown lifecycle state %q (available: %v)", newState, mapKeys(c.optionIDs))
	}

	// Step 1: Find the project item ID for this issue
	itemID, err := c.findProjectItem(repo, issueNumber)
	if err != nil {
		return fmt.Errorf("find project item for %s#%d: %w", repo, issueNumber, err)
	}

	// Step 2: Update the Lifecycle field
	mutation := `mutation($projectId: ID!, $itemId: ID!, $fieldId: ID!, $optionId: String!) {
		updateProjectV2ItemFieldValue(input: {
			projectId: $projectId
			itemId: $itemId
			fieldId: $fieldId
			value: { singleSelectOptionId: $optionId }
		}) {
			projectV2Item { id }
		}
	}`

	vars := map[string]any{
		"projectId": c.projectID,
		"itemId":    itemID,
		"fieldId":   c.fieldID,
		"optionId":  optionID,
	}

	if _, err := c.graphQL(mutation, vars); err != nil {
		return fmt.Errorf("update lifecycle field: %w", err)
	}

	return nil
}

func (c *GitHubClient) findProjectItem(repo string, issueNumber int) (string, error) {
	query := `query($org: String!, $repo: String!, $number: Int!) {
		repository(owner: $org, name: $repo) {
			issue(number: $number) {
				projectItems(first: 10) {
					nodes {
						id
						project { id }
					}
				}
			}
		}
	}`

	vars := map[string]any{
		"org":    c.org,
		"repo":   repo,
		"number": issueNumber,
	}

	resp, err := c.graphQL(query, vars)
	if err != nil {
		return "", err
	}

	var result struct {
		Repository struct {
			Issue struct {
				ProjectItems struct {
					Nodes []struct {
						ID      string `json:"id"`
						Project struct {
							ID string `json:"id"`
						} `json:"project"`
					} `json:"nodes"`
				} `json:"projectItems"`
			} `json:"issue"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return "", fmt.Errorf("parse project items: %w", err)
	}

	for _, item := range result.Repository.Issue.ProjectItems.Nodes {
		if item.Project.ID == c.projectID {
			return item.ID, nil
		}
	}

	return "", fmt.Errorf("issue %s#%d is not on project %s", repo, issueNumber, c.projectID)
}

// CreateBugIssue creates a new bug Issue linked to a feature Issue when it transitions to dead.
func (c *GitHubClient) CreateBugIssue(repo string, featureIssueNumber int, alert Alert) (string, error) {
	title := fmt.Sprintf("[Bug] %s is dead — %s", alert.Labels["alertname"], alert.Annotations["summary"])
	body := fmt.Sprintf(`## Auto-created by health-bridge

**Feature Issue:** %s/%s#%d
**Alert:** %s
**Severity:** %s
**Summary:** %s
**Started:** %s

This feature has been marked as **dead** by the health monitoring system.

[View in Grafana](%s)

---
*Automated by health-bridge on dead transition*`,
		c.org, repo, featureIssueNumber,
		alert.Labels["alertname"],
		alert.Labels["severity"],
		alert.Annotations["summary"],
		alert.StartsAt,
		alert.GeneratorURL,
	)

	payload, err := json.Marshal(map[string]any{
		"title":  title,
		"body":   body,
		"labels": []string{"bug"},
	})
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("%s/repos/%s/%s/issues", githubRESTURL, c.org, repo)
	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("github API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	return result.HTMLURL, nil
}

// HasOpenBug checks whether an open bug issue already exists for a given alert name.
// Used as a restart safety net — prevents duplicate bug creation when in-memory state is lost.
func (c *GitHubClient) HasOpenBug(repo, alertName string) (bool, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/issues?labels=bug&state=open&per_page=50", githubRESTURL, c.org, repo)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("github API returned %d: %s", resp.StatusCode, string(body))
	}

	var issues []struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(body, &issues); err != nil {
		return false, fmt.Errorf("parse issues: %w", err)
	}

	prefix := fmt.Sprintf("[Bug] %s is dead", alertName)
	for _, issue := range issues {
		if len(issue.Title) >= len(prefix) && issue.Title[:len(prefix)] == prefix {
			return true, nil
		}
	}

	return false, nil
}

// AddIssueComment adds a comment to a GitHub Issue via REST API.
func (c *GitHubClient) AddIssueComment(repo string, issueNumber int, body string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", githubRESTURL, c.org, repo, issueNumber)

	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github API returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

func (c *GitHubClient) graphQL(query string, variables map[string]any) (json.RawMessage, error) {
	reqBody := graphQLRequest{Query: query, Variables: variables}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", githubGraphQLURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github API returned %d: %s", resp.StatusCode, string(body))
	}

	var gqlResp graphQLResponse
	if err := json.Unmarshal(body, &gqlResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if len(gqlResp.Errors) > 0 {
		return nil, fmt.Errorf("graphql errors: %v", gqlResp.Errors[0].Message)
	}

	return gqlResp.Data, nil
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
