package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// Bridge is the core service that processes Grafana alerts and updates GitHub.
type Bridge struct {
	github *GitHubClient
}

// GrafanaPayload is the webhook payload sent by Grafana alerting.
type GrafanaPayload struct {
	Status string  `json:"status"`
	Alerts []Alert `json:"alerts"`
}

// Alert is a single alert within a Grafana webhook payload.
type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	EndsAt       string            `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// NewBridge creates a Bridge and loads GitHub Project metadata.
func NewBridge(token, org string, projectNumber int) (*Bridge, error) {
	gh, err := NewGitHubClient(token, org, projectNumber)
	if err != nil {
		return nil, fmt.Errorf("github client init: %w", err)
	}
	return &Bridge{github: gh}, nil
}

// Ready returns true if the bridge has loaded project metadata.
func (b *Bridge) Ready() bool {
	return b.github != nil && b.github.projectID != ""
}

// WebhookHandler returns an HTTP handler that validates the webhook secret
// and processes Grafana alerts.
func (b *Bridge) WebhookHandler(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		// Validate webhook secret via Authorization header (Bearer token)
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer "+secret {
			log.Printf("Unauthorized webhook request from %s", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var payload GrafanaPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			log.Printf("Invalid JSON payload: %v", err)
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		processed := 0
		for _, alert := range payload.Alerts {
			if err := b.processAlert(alert); err != nil {
				log.Printf("Error processing alert %s: %v", alert.Labels["alertname"], err)
				continue
			}
			processed++
		}

		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"processed": %d, "total": %d}`, processed, len(payload.Alerts))
	}
}

func (b *Bridge) processAlert(alert Alert) error {
	issueRef := alert.Labels["github_issue"]
	if issueRef == "" {
		log.Printf("Alert %s has no github_issue label, skipping", alert.Labels["alertname"])
		return nil
	}

	repo, number, err := ParseIssueRef(issueRef)
	if err != nil {
		return fmt.Errorf("parse issue ref %q: %w", issueRef, err)
	}

	newState := MapAlertToState(alert.Status, alert.Labels["severity"])

	// Update lifecycle state on project board
	if err := b.github.UpdateLifecycleState(repo, number, newState); err != nil {
		return fmt.Errorf("update lifecycle %s → %s: %w", issueRef, newState, err)
	}

	// Add comment to issue with alert context
	comment := FormatComment(alert, newState)
	if err := b.github.AddIssueComment(repo, number, comment); err != nil {
		log.Printf("Warning: failed to add comment to %s (state update succeeded): %v", issueRef, err)
		// Non-fatal: the state update is the critical action
	}

	// On dead transition, create a bug Issue linked to the feature Issue
	if newState == "dead" {
		bugURL, err := b.github.CreateBugIssue(repo, number, alert)
		if err != nil {
			log.Printf("Warning: failed to create bug issue for %s: %v", issueRef, err)
		} else {
			log.Printf("Created bug issue: %s", bugURL)
		}
	}

	log.Printf("Processed: %s → %s (alert: %s, status: %s)", issueRef, newState, alert.Labels["alertname"], alert.Status)
	return nil
}

// ParseIssueRef parses a "repo#number" string into repo name and issue number.
func ParseIssueRef(ref string) (repo string, number int, err error) {
	parts := strings.SplitN(ref, "#", 2)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("expected format 'repo#number', got %q", ref)
	}
	repo = parts[0]
	number, err = strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, fmt.Errorf("invalid issue number %q: %w", parts[1], err)
	}
	if repo == "" || number <= 0 {
		return "", 0, fmt.Errorf("invalid issue ref: repo=%q number=%d", repo, number)
	}
	return repo, number, nil
}

// MapAlertToState maps Grafana alert status and severity to a lifecycle state.
func MapAlertToState(alertStatus, severity string) string {
	switch alertStatus {
	case "resolved":
		return "healthy"
	case "firing":
		switch severity {
		case "critical":
			return "dead"
		case "warning":
			return "degraded"
		default:
			return "degraded" // Default firing alerts to degraded
		}
	default:
		return "degraded" // Unknown status defaults to degraded
	}
}

// FormatComment creates a markdown comment for a GitHub Issue describing the alert.
func FormatComment(alert Alert, newState string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Health Bridge: `%s`\n\n", newState))
	sb.WriteString(fmt.Sprintf("**Alert:** %s\n", alert.Labels["alertname"]))
	sb.WriteString(fmt.Sprintf("**Status:** %s\n", alert.Status))
	sb.WriteString(fmt.Sprintf("**Severity:** %s\n", alert.Labels["severity"]))
	if summary := alert.Annotations["summary"]; summary != "" {
		sb.WriteString(fmt.Sprintf("**Summary:** %s\n", summary))
	}
	if desc := alert.Annotations["description"]; desc != "" {
		sb.WriteString(fmt.Sprintf("**Description:** %s\n", desc))
	}
	sb.WriteString(fmt.Sprintf("**Started:** %s\n", alert.StartsAt))
	if alert.Status == "resolved" && alert.EndsAt != "" {
		sb.WriteString(fmt.Sprintf("**Resolved:** %s\n", alert.EndsAt))
	}
	if alert.GeneratorURL != "" {
		sb.WriteString(fmt.Sprintf("\n[View in Grafana](%s)\n", alert.GeneratorURL))
	}
	sb.WriteString("\n---\n*Automated by health-bridge*\n")
	return sb.String()
}
