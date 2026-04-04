package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseIssueRef(t *testing.T) {
	tests := []struct {
		input      string
		wantRepo   string
		wantNumber int
		wantErr    bool
	}{
		{"frank#8", "frank", 8, false},
		{"willikins#11", "willikins", 11, false},
		{"content-factory#1", "content-factory", 1, false},
		{"nohash", "", 0, true},
		{"#5", "", 0, true},
		{"repo#0", "", 0, true},
		{"repo#abc", "", 0, true},
		{"", "", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			repo, number, err := ParseIssueRef(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseIssueRef(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if repo != tt.wantRepo {
				t.Errorf("ParseIssueRef(%q) repo = %q, want %q", tt.input, repo, tt.wantRepo)
			}
			if number != tt.wantNumber {
				t.Errorf("ParseIssueRef(%q) number = %d, want %d", tt.input, number, tt.wantNumber)
			}
		})
	}
}

func TestMapAlertToState(t *testing.T) {
	tests := []struct {
		status   string
		severity string
		want     string
	}{
		{"resolved", "critical", "healthy"},
		{"resolved", "warning", "healthy"},
		{"resolved", "", "healthy"},
		{"firing", "critical", "dead"},
		{"firing", "warning", "degraded"},
		{"firing", "", "degraded"},
		{"unknown", "", "degraded"},
	}

	for _, tt := range tests {
		t.Run(tt.status+"_"+tt.severity, func(t *testing.T) {
			got := MapAlertToState(tt.status, tt.severity)
			if got != tt.want {
				t.Errorf("MapAlertToState(%q, %q) = %q, want %q", tt.status, tt.severity, got, tt.want)
			}
		})
	}
}

func TestFormatComment(t *testing.T) {
	alert := Alert{
		Status:       "firing",
		Labels:       map[string]string{"alertname": "exercise-reminder-stale", "severity": "critical"},
		Annotations:  map[string]string{"summary": "Exercise reminder heartbeat stale"},
		StartsAt:     "2026-04-04T10:00:00Z",
		GeneratorURL: "https://grafana.frank.derio.net/alerting/grafana/exercise-reminder-stale/view",
	}

	comment := FormatComment(alert, "dead")

	if !bytes.Contains([]byte(comment), []byte("## Health Bridge: `dead`")) {
		t.Error("Comment should contain the state header")
	}
	if !bytes.Contains([]byte(comment), []byte("exercise-reminder-stale")) {
		t.Error("Comment should contain the alert name")
	}
	if !bytes.Contains([]byte(comment), []byte("View in Grafana")) {
		t.Error("Comment should contain Grafana link")
	}
}

func TestWebhookHandler_Unauthorized(t *testing.T) {
	bridge := &Bridge{github: &GitHubClient{projectID: "test"}}
	handler := bridge.WebhookHandler("correct-secret")

	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader([]byte("{}")))
	req.Header.Set("Authorization", "Bearer wrong-secret")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401, got %d", w.Code)
	}
}

func TestWebhookHandler_ValidPayload(t *testing.T) {
	// Create a mock GitHub API server
	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	}))
	defer mockGH.Close()

	bridge := &Bridge{
		github: &GitHubClient{
			projectID:  "test-project",
			fieldID:    "test-field",
			optionIDs:  map[string]string{"healthy": "opt-1", "dead": "opt-2", "degraded": "opt-3"},
			httpClient: mockGH.Client(),
		},
	}

	// Payload with no github_issue label — should process without error
	payload := GrafanaPayload{
		Status: "firing",
		Alerts: []Alert{
			{
				Status: "firing",
				Labels: map[string]string{"alertname": "test", "severity": "warning"},
			},
		},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-secret")
	w := httptest.NewRecorder()

	handler := bridge.WebhookHandler("test-secret")
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d", w.Code)
	}
}
