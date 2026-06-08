package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Bridge is the core service that processes Grafana alerts and updates GitHub.
type Bridge struct {
	github    *GitHubClient
	lastState map[string]string // issueRef → last lifecycle state (dedup)
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
	return &Bridge{github: gh, lastState: make(map[string]string)}, nil
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

	// Blindness ≠ death: a firing DatasourceError/NoData means we lost sight of
	// the layer, not that it died. Cap it at degraded and never manufacture a
	// bug from it (the 2026-06-08 power-outage storm fabricated 5 such bugs).
	blind := isBlindAlert(alert.Labels["alertname"])
	if alert.Status == "firing" && blind && newState == "dead" {
		newState = "degraded"
	}

	// Update lifecycle state on project board (idempotent — always safe)
	if err := b.github.UpdateLifecycleState(repo, number, newState); err != nil {
		return fmt.Errorf("update lifecycle %s → %s: %w", issueRef, newState, err)
	}

	// On resolved, auto-close any open bug Issues this alert created.
	// Deliberately BEFORE the lastState dedup (dedup is keyed per tracker,
	// shared by multiple alerts — a deduped repeat-resolved must still close;
	// the operation is idempotent: no open bugs ⇒ no-op) and NOT gated on
	// severity (a severity-label edit between fire and resolve must not
	// strand a bug).
	if alert.Status == "resolved" {
		alertName := alert.Labels["alertname"]
		// Match by feature ref alone (alertname-agnostic): the resolve often
		// arrives under a different alertname than the one that created the bug
		// (e.g. a "DatasourceError" bug healed by the real per-rule resolve).
		bugs, err := b.github.FindOpenBugsByFeature(repo, number)
		if err != nil {
			log.Printf("Warning: failed to find open bugs for %s: %v", issueRef, err)
		}
		for _, n := range bugs {
			if err := b.github.CloseBugIssue(repo, n, FormatHealComment(alert)); err != nil {
				log.Printf("Warning: failed to close bug %s#%d: %v", repo, n, err)
			} else {
				log.Printf("Closed bug issue %s#%d (alert %s resolved)", repo, n, alertName)
			}
		}
	}

	// Dedup: only comment and create bugs on actual state transitions
	prevState := b.lastState[issueRef]
	b.lastState[issueRef] = newState

	if prevState == newState {
		log.Printf("Dedup: %s already %s, skipping comment/bug", issueRef, newState)
		return nil
	}

	// Add comment to issue with alert context
	comment := FormatComment(alert, newState)
	if err := b.github.AddIssueComment(repo, number, comment); err != nil {
		log.Printf("Warning: failed to add comment to %s (state update succeeded): %v", issueRef, err)
	}

	// On dead transition, create a bug Issue linked to the feature Issue.
	// Blind alerts never reach here (capped at degraded above); the !blind
	// guard documents that invariant.
	if newState == "dead" && !blind {
		// Safety net: check for existing open bug before creating (handles restarts).
		// Feature-ref-aware match: layers sharing an alertname (DatasourceError)
		// must not suppress each other's bugs.
		alertName := alert.Labels["alertname"]
		existing, err := b.github.FindOpenBugs(repo, alertName, number)
		if err != nil {
			log.Printf("Warning: failed to check for existing bug for %s: %v", issueRef, err)
			// Fall through to create — better a duplicate than a missing bug
		}
		if len(existing) > 0 {
			log.Printf("Dedup: open bug already exists for %s in %s, skipping creation", alertName, repo)
		} else {
			bugURL, err := b.github.CreateBugIssue(repo, number, alert)
			if err != nil {
				log.Printf("Warning: failed to create bug issue for %s: %v", issueRef, err)
			} else {
				log.Printf("Created bug issue: %s", bugURL)
			}
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

// isBlindAlert reports whether the alert name means "monitoring cannot see this
// layer" (Grafana built-ins raised when a query errors or returns no data)
// rather than a confirmed fault. Such alerts must not be treated as death:
// they carry the affected rule's github_issue label but their templates can't
// resolve real data (summaries render "[no value]").
func isBlindAlert(alertname string) bool {
	switch alertname {
	case "DatasourceError", "NoData":
		return true
	}
	return false
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

// FormatHealComment creates the markdown comment posted on a bug Issue when
// its alert resolves and the bug is auto-closed. The outage duration is
// computed from the resolved alert's StartsAt→EndsAt and omitted when either
// timestamp fails to parse.
func FormatHealComment(alert Alert) string {
	var sb strings.Builder
	sb.WriteString("## Health Bridge: auto-close\n\n")
	sb.WriteString(fmt.Sprintf("**Alert:** %s\n", alert.Labels["alertname"]))
	sb.WriteString(fmt.Sprintf("**Severity:** %s\n", alert.Labels["severity"]))
	if summary := alert.Annotations["summary"]; summary != "" {
		sb.WriteString(fmt.Sprintf("**Summary:** %s\n", summary))
	}
	sb.WriteString(fmt.Sprintf("**Resolved:** %s\n", alert.EndsAt))
	start, errStart := time.Parse(time.RFC3339, alert.StartsAt)
	end, errEnd := time.Parse(time.RFC3339, alert.EndsAt)
	if errStart == nil && errEnd == nil && !end.Before(start) {
		sb.WriteString(fmt.Sprintf("**Outage duration:** %s\n", end.Sub(start).Round(time.Minute)))
	}
	sb.WriteString("\nSystem healed — closing this bug automatically.\n")
	sb.WriteString("\n---\n*Automated by health-bridge*\n")
	return sb.String()
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
