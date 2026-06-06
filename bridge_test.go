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

func TestProcessAlert_DedupSkipsRepeatState(t *testing.T) {
	// Track what the mock GitHub server receives
	var commentCount, bugCount int

	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GraphQL requests (lifecycle updates)
		if r.URL.Path == "/graphql" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"issue": map[string]any{
							"projectItems": map[string]any{
								"nodes": []map[string]any{
									{"id": "item-1", "project": map[string]any{"id": "proj-1"}},
								},
							},
						},
					},
					"updateProjectV2ItemFieldValue": map[string]any{
						"projectV2Item": map[string]any{"id": "item-1"},
					},
				},
			})
			return
		}

		// REST: issue comments
		if r.Method == "POST" && r.URL.Path == "/repos/derio-net/willikins/issues/11/comments" {
			commentCount++
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
			return
		}

		// REST: bug issue search (GET issues with bug label)
		if r.Method == "GET" && r.URL.Path == "/repos/derio-net/willikins/issues" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode([]map[string]any{})
			return
		}

		// REST: create bug issue
		if r.Method == "POST" && r.URL.Path == "/repos/derio-net/willikins/issues" {
			bugCount++
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"html_url": "https://github.com/derio-net/willikins/issues/99"})
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer mockGH.Close()

	bridge := &Bridge{
		github: &GitHubClient{
			token:         "test",
			org:           "derio-net",
			projectID:     "proj-1",
			fieldID:       "field-1",
			optionIDs:     map[string]string{"healthy": "opt-1", "dead": "opt-2", "degraded": "opt-3"},
			httpClient:    mockGH.Client(),
			projectNumber: 1,
		},
		lastState: make(map[string]string),
	}

	// Override the GitHub API URLs to point at mock
	origGraphQL := githubGraphQLURL
	origREST := githubRESTURL
	defer func() {
		setGitHubURLs(origGraphQL, origREST)
	}()
	setGitHubURLs(mockGH.URL+"/graphql", mockGH.URL)

	alert := Alert{
		Status:      "firing",
		Labels:      map[string]string{"alertname": "Exercise Reminder Stale", "severity": "critical", "github_issue": "willikins#11"},
		Annotations: map[string]string{"summary": "Exercise reminder heartbeat is stale"},
		StartsAt:    "2026-04-05T02:00:00Z",
		Fingerprint: "abc123",
	}

	// First call: should process fully (comment + bug)
	if err := bridge.processAlert(alert); err != nil {
		t.Fatalf("First processAlert failed: %v", err)
	}
	if commentCount != 1 {
		t.Errorf("First call: expected 1 comment, got %d", commentCount)
	}
	if bugCount != 1 {
		t.Errorf("First call: expected 1 bug, got %d", bugCount)
	}

	// Second call with same state: should be deduped (no new comment or bug)
	if err := bridge.processAlert(alert); err != nil {
		t.Fatalf("Second processAlert failed: %v", err)
	}
	if commentCount != 1 {
		t.Errorf("Second call: expected still 1 comment, got %d", commentCount)
	}
	if bugCount != 1 {
		t.Errorf("Second call: expected still 1 bug, got %d", bugCount)
	}

	// Third call with resolved state: should process (state transition)
	alert.Status = "resolved"
	if err := bridge.processAlert(alert); err != nil {
		t.Fatalf("Third processAlert failed: %v", err)
	}
	if commentCount != 2 {
		t.Errorf("Third call: expected 2 comments, got %d", commentCount)
	}
	// No bug for healthy state
	if bugCount != 1 {
		t.Errorf("Third call: expected still 1 bug, got %d", bugCount)
	}
}

func TestFormatHealComment(t *testing.T) {
	alert := Alert{
		Status:      "resolved",
		Labels:      map[string]string{"alertname": "Layer 8 Observability Degraded", "severity": "critical"},
		Annotations: map[string]string{"summary": "L8 Observability: pod/fluent-bit-znsw9 failing"},
		StartsAt:    "2026-06-04T16:52:58Z",
		EndsAt:      "2026-06-04T17:38:03Z",
	}

	comment := FormatHealComment(alert)

	for _, want := range []string{
		"## Health Bridge: auto-close",
		"Layer 8 Observability Degraded",
		"2026-06-04T17:38:03Z",
		"45m", // 16:52:58 → 17:38:03 rounds to 45m
		"System healed — closing this bug automatically.",
		"*Automated by health-bridge*",
	} {
		if !bytes.Contains([]byte(comment), []byte(want)) {
			t.Errorf("heal comment missing %q\n---\n%s", want, comment)
		}
	}

	// Unparseable timestamps: comment still renders, duration omitted.
	alert.StartsAt = "not-a-timestamp"
	comment = FormatHealComment(alert)
	if bytes.Contains([]byte(comment), []byte("Outage duration")) {
		t.Error("duration must be omitted when StartsAt does not parse")
	}
	if !bytes.Contains([]byte(comment), []byte("Layer 8 Observability Degraded")) {
		t.Error("comment must still render without parseable timestamps")
	}
}

func TestFindOpenBugs(t *testing.T) {
	// Fixtures mirror the real frank-ops shape: Grafana's synthetic
	// DatasourceError alertname is shared across layers, so matching must
	// disambiguate via the Feature Issue ref in the bug body.
	fixtures := []map[string]any{
		{
			"number": 38,
			"title":  "[Bug] DatasourceError is dead — L24 Traefik: pod [no value] NotReady",
			"body":   "## Auto-created by health-bridge\n\n**Feature Issue:** derio-net/frank-ops#24\n**Alert:** DatasourceError\n",
		},
		{
			"number": 39,
			"title":  "[Bug] DatasourceError is dead — L8 Observability: [no value] failing",
			"body":   "## Auto-created by health-bridge\n\n**Feature Issue:** derio-net/frank-ops#8\n**Alert:** DatasourceError\n",
		},
		{
			"number": 41,
			"title":  "[Bug] DatasourceError is dead — L2 OS: [no value] failing",
			"body":   "## Auto-created by health-bridge\n\n**Feature Issue:** derio-net/frank-ops#2\n**Alert:** DatasourceError\n",
		},
		{
			// Historical duplicate for the same alert+feature as #38 —
			// must also be returned so auto-close clears all of them.
			"number": 45,
			"title":  "[Bug] DatasourceError is dead — L24 Traefik: pod [no value] NotReady",
			"body":   "## Auto-created by health-bridge\n\n**Feature Issue:** derio-net/frank-ops#24\n**Alert:** DatasourceError\n",
		},
	}

	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/repos/derio-net/frank-ops/issues" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(fixtures)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockGH.Close()

	origGraphQL := githubGraphQLURL
	origREST := githubRESTURL
	defer func() { setGitHubURLs(origGraphQL, origREST) }()
	setGitHubURLs(mockGH.URL+"/graphql", mockGH.URL)

	client := &GitHubClient{
		token:      "test",
		org:        "derio-net",
		httpClient: mockGH.Client(),
	}

	tests := []struct {
		name          string
		alertName     string
		featureNumber int
		want          []int
	}{
		{"matches only the right layer's bug", "DatasourceError", 24, []int{38, 45}},
		{"newline-terminated needle: #2 must not match #24", "DatasourceError", 2, []int{41}},
		{"no match for other alert names", "OtherAlert", 24, nil},
		{"no match for unknown feature number", "DatasourceError", 99, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := client.FindOpenBugs("frank-ops", tt.alertName, tt.featureNumber)
			if err != nil {
				t.Fatalf("FindOpenBugs error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("FindOpenBugs = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("FindOpenBugs = %v, want %v", got, tt.want)
					break
				}
			}
		})
	}
}

func TestCloseBugIssue(t *testing.T) {
	var gotComment string
	var patchBody map[string]string
	var commentBeforePatch bool

	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/repos/derio-net/frank-ops/issues/38/comments":
			var payload map[string]string
			json.NewDecoder(r.Body).Decode(&payload)
			gotComment = payload["body"]
			commentBeforePatch = patchBody == nil
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == "PATCH" && r.URL.Path == "/repos/derio-net/frank-ops/issues/38":
			json.NewDecoder(r.Body).Decode(&patchBody)
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"number": 38})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockGH.Close()

	origGraphQL := githubGraphQLURL
	origREST := githubRESTURL
	defer func() { setGitHubURLs(origGraphQL, origREST) }()
	setGitHubURLs(mockGH.URL+"/graphql", mockGH.URL)

	client := &GitHubClient{token: "test", org: "derio-net", httpClient: mockGH.Client()}

	if err := client.CloseBugIssue("frank-ops", 38, "healed comment"); err != nil {
		t.Fatalf("CloseBugIssue error: %v", err)
	}
	if gotComment != "healed comment" {
		t.Errorf("comment body = %q, want %q", gotComment, "healed comment")
	}
	if !commentBeforePatch {
		t.Error("heal comment must be posted before the close PATCH")
	}
	if patchBody["state"] != "closed" || patchBody["state_reason"] != "completed" {
		t.Errorf("PATCH body = %v, want state=closed state_reason=completed", patchBody)
	}
}

func TestCloseBugIssue_CommentFailureStillCloses(t *testing.T) {
	var patched bool

	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/repos/derio-net/frank-ops/issues/38/comments":
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == "PATCH" && r.URL.Path == "/repos/derio-net/frank-ops/issues/38":
			patched = true
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"number": 38})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockGH.Close()

	origGraphQL := githubGraphQLURL
	origREST := githubRESTURL
	defer func() { setGitHubURLs(origGraphQL, origREST) }()
	setGitHubURLs(mockGH.URL+"/graphql", mockGH.URL)

	client := &GitHubClient{token: "test", org: "derio-net", httpClient: mockGH.Client()}

	if err := client.CloseBugIssue("frank-ops", 38, "healed comment"); err != nil {
		t.Fatalf("CloseBugIssue must tolerate comment failure, got error: %v", err)
	}
	if !patched {
		t.Error("close PATCH must still be sent when the comment POST fails")
	}
}

func TestProcessAlert_ResolvedClosesOpenBugs(t *testing.T) {
	openBugs := []map[string]any{
		{
			"number": 99,
			"title":  "[Bug] Exercise Reminder Stale is dead — heartbeat stale",
			"body":   "## Auto-created by health-bridge\n\n**Feature Issue:** derio-net/willikins#11\n**Alert:** Exercise Reminder Stale\n",
		},
	}
	var bugCommentCount, patchCount int

	mockGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/graphql":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"issue": map[string]any{
							"projectItems": map[string]any{
								"nodes": []map[string]any{
									{"id": "item-1", "project": map[string]any{"id": "proj-1"}},
								},
							},
						},
					},
					"updateProjectV2ItemFieldValue": map[string]any{
						"projectV2Item": map[string]any{"id": "item-1"},
					},
				},
			})
		case r.Method == "GET" && r.URL.Path == "/repos/derio-net/willikins/issues":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(openBugs)
		case r.Method == "POST" && r.URL.Path == "/repos/derio-net/willikins/issues/99/comments":
			bugCommentCount++
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == "PATCH" && r.URL.Path == "/repos/derio-net/willikins/issues/99":
			patchCount++
			openBugs = []map[string]any{} // closing empties the open-bug list
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"number": 99})
		case r.Method == "POST" && r.URL.Path == "/repos/derio-net/willikins/issues/11/comments":
			// tracker transition comment — uninteresting here
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 2})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockGH.Close()

	origGraphQL := githubGraphQLURL
	origREST := githubRESTURL
	defer func() { setGitHubURLs(origGraphQL, origREST) }()
	setGitHubURLs(mockGH.URL+"/graphql", mockGH.URL)

	bridge := &Bridge{
		github: &GitHubClient{
			token:         "test",
			org:           "derio-net",
			projectID:     "proj-1",
			fieldID:       "field-1",
			optionIDs:     map[string]string{"healthy": "opt-1", "dead": "opt-2", "degraded": "opt-3"},
			httpClient:    mockGH.Client(),
			projectNumber: 1,
		},
		lastState: make(map[string]string),
	}

	alert := Alert{
		Status:      "resolved",
		Labels:      map[string]string{"alertname": "Exercise Reminder Stale", "severity": "critical", "github_issue": "willikins#11"},
		Annotations: map[string]string{"summary": "Exercise reminder heartbeat is stale"},
		StartsAt:    "2026-06-04T16:52:58Z",
		EndsAt:      "2026-06-04T17:38:03Z",
	}

	// First resolved: must close the matching open bug (comment + PATCH).
	if err := bridge.processAlert(alert); err != nil {
		t.Fatalf("First processAlert failed: %v", err)
	}
	if bugCommentCount != 1 {
		t.Errorf("First resolved: expected 1 heal comment on bug, got %d", bugCommentCount)
	}
	if patchCount != 1 {
		t.Errorf("First resolved: expected 1 close PATCH, got %d", patchCount)
	}

	// Repeat resolved (lastState dedup now reports repeat state): the close
	// path must still run — and be a clean no-op with no bugs left open.
	if err := bridge.processAlert(alert); err != nil {
		t.Fatalf("Second processAlert failed: %v", err)
	}
	if patchCount != 1 {
		t.Errorf("Repeat resolved: expected still 1 close PATCH (idempotent no-op), got %d", patchCount)
	}

	// Resolved for an alert with no open bugs: no-op.
	alert.Labels["alertname"] = "Some Other Alert"
	if err := bridge.processAlert(alert); err != nil {
		t.Fatalf("Third processAlert failed: %v", err)
	}
	if patchCount != 1 {
		t.Errorf("No-bug resolved: expected still 1 close PATCH, got %d", patchCount)
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
