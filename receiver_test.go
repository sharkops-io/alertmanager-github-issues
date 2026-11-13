package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub is an in-memory issues API for one repository.
type fakeGitHub struct {
	mu       sync.Mutex
	issues   []*fakeIssue
	comments map[int][]string
	pageSize int
	failNext bool
	tokens   []string // Authorization headers seen
}

type fakeIssue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	State  string   `json:"state"`
	Reason string   `json:"state_reason"`
	Labels []string `json:"-"`
}

func newFake() *fakeGitHub {
	return &fakeGitHub{comments: map[int][]string{}, pageSize: 100}
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, r.Header.Get("Authorization"))
	if f.failNext {
		f.failNext = false
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// repos/o/r/issues[/n[/comments]]
	if len(parts) < 4 || parts[0] != "repos" || parts[3] != "issues" {
		http.NotFound(w, r)
		return
	}
	switch {
	case len(parts) == 4 && r.Method == http.MethodGet:
		label := r.URL.Query().Get("labels")
		var open []map[string]any
		for _, is := range f.issues {
			if is.State == "open" && contains(is.Labels, label) {
				open = append(open, map[string]any{"number": is.Number, "title": is.Title, "body": is.Body})
			}
		}
		// A pull request in the listing must be ignored.
		open = append(open, map[string]any{"number": 9999, "body": "", "pull_request": map[string]any{}})
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 0 {
			page = 1
		}
		start := (page - 1) * f.pageSize
		end := min(start+f.pageSize, len(open))
		if start > len(open) {
			start = len(open)
		}
		if end < len(open) {
			next := *r.URL
			q := next.Query()
			q.Set("page", strconv.Itoa(page+1))
			next.RawQuery = q.Encode()
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s>; rel="next"`, r.Host, next.String()))
		}
		json.NewEncoder(w).Encode(open[start:end])
	case len(parts) == 4 && r.Method == http.MethodPost:
		var in struct {
			Title  string   `json:"title"`
			Body   string   `json:"body"`
			Labels []string `json:"labels"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		is := &fakeIssue{Number: len(f.issues) + 1, Title: in.Title, Body: in.Body, State: "open", Labels: in.Labels}
		f.issues = append(f.issues, is)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(is)
	case len(parts) == 5 && r.Method == http.MethodPatch:
		n, _ := strconv.Atoi(parts[4])
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		is := f.issues[n-1]
		if v, ok := in["body"]; ok {
			is.Body = v
		}
		if v, ok := in["state"]; ok {
			is.State = v
			is.Reason = in["state_reason"]
		}
		json.NewEncoder(w).Encode(is)
	case len(parts) == 6 && parts[5] == "comments" && r.Method == http.MethodPost:
		n, _ := strconv.Atoi(parts[4])
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		f.comments[n] = append(f.comments[n], in["body"])
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{}`)
	default:
		http.NotFound(w, r)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func setup(t *testing.T) (*fakeGitHub, *httptest.Server, *Receiver) {
	t.Helper()
	f := newFake()
	gh := httptest.NewServer(f)
	t.Cleanup(gh.Close)
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("test-token\n"), 0o600)
	r := &Receiver{
		gh:            &GitHub{apiURL: gh.URL, owner: "o", repo: "r", tokens: fileToken{path: tok}, client: gh.Client()},
		label:         "alert",
		labelFromKeys: []string{"severity"},
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:           func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	}
	return f, gh, r
}

func msg(status string, alerts ...Alert) []byte {
	m := Message{
		Version:     "4",
		GroupKey:    `{}:{alertname="DiskFull", namespace="db"}`,
		Status:      status,
		Receiver:    "github",
		GroupLabels: map[string]string{"alertname": "DiskFull", "namespace": "db"},
		CommonLabels: map[string]string{
			"alertname": "DiskFull", "namespace": "db", "severity": "warning",
		},
		CommonAnnotations: map[string]string{"summary": "disk is filling"},
		ExternalURL:       "http://alertmanager.example.com",
		Alerts:            alerts,
	}
	b, _ := json.Marshal(m)
	return b
}

func alert(status, pod string) Alert {
	return Alert{
		Status:      status,
		Labels:      map[string]string{"alertname": "DiskFull", "namespace": "db", "pod": pod, "severity": "warning"},
		Annotations: map[string]string{"description": "pod " + pod + " is at 95%"},
		StartsAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func post(t *testing.T, r http.Handler, body []byte) int {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body)))
	return rec.Code
}

func TestLifecycle(t *testing.T) {
	f, _, r := setup(t)

	if c := post(t, r, msg("firing", alert("firing", "db-0"))); c != 200 {
		t.Fatalf("firing: %d", c)
	}
	if len(f.issues) != 1 {
		t.Fatalf("want 1 issue, got %d", len(f.issues))
	}
	is := f.issues[0]
	if is.Title != "DiskFull (namespace=db)" {
		t.Errorf("title = %q", is.Title)
	}
	if !contains(is.Labels, "alert") || !contains(is.Labels, "severity:warning") {
		t.Errorf("labels = %v", is.Labels)
	}
	if f.tokens[0] != "token test-token" {
		t.Errorf("auth header = %q", f.tokens[0])
	}

	// A repeat notification with the same alerts changes nothing.
	post(t, r, msg("firing", alert("firing", "db-0")))
	if len(f.issues) != 1 || r.updated.Load() != 0 {
		t.Fatalf("repeat created or updated: issues=%d updated=%d", len(f.issues), r.updated.Load())
	}

	// A second alert joining the group updates the body of the same issue.
	post(t, r, msg("firing", alert("firing", "db-0"), alert("firing", "db-1")))
	if len(f.issues) != 1 || !strings.Contains(f.issues[0].Body, "db-1") {
		t.Fatalf("second alert not reflected: issues=%d", len(f.issues))
	}

	// Resolution comments and closes it.
	post(t, r, msg("resolved", alert("resolved", "db-0"), alert("resolved", "db-1")))
	if is.State != "closed" || is.Reason != "completed" {
		t.Fatalf("state = %s/%s", is.State, is.Reason)
	}
	if len(f.comments[1]) != 1 || !strings.Contains(f.comments[1][0], "2026-01-02T03:04:05Z") {
		t.Errorf("comments = %v", f.comments[1])
	}

	// Firing again later is a new incident: a new issue.
	post(t, r, msg("firing", alert("firing", "db-0")))
	if len(f.issues) != 2 {
		t.Fatalf("refire: want 2 issues, got %d", len(f.issues))
	}
}

func TestResolvedWithoutIssueIsNoop(t *testing.T) {
	f, _, r := setup(t)
	if c := post(t, r, msg("resolved", alert("resolved", "db-0"))); c != 200 {
		t.Fatalf("code %d", c)
	}
	if len(f.issues) != 0 {
		t.Fatal("resolved created an issue")
	}
}

func TestOpenIssueFoundBeyondFirstPage(t *testing.T) {
	f, _, r := setup(t)
	f.pageSize = 2
	for i := 0; i < 5; i++ {
		f.issues = append(f.issues, &fakeIssue{Number: i + 1, Body: "other", State: "open", Labels: []string{"alert"}})
	}
	// Ours becomes issue 6, on page 3 of the listing.
	post(t, r, msg("firing", alert("firing", "db-0")))
	if len(f.issues) != 6 {
		t.Fatalf("want 6 issues, got %d", len(f.issues))
	}
	ours := f.issues[5]
	post(t, r, msg("resolved", alert("resolved", "db-0")))
	if ours.State != "closed" {
		t.Fatal("issue on a later page was not found, so not closed")
	}
}

func TestGitHubFailureIsA5xx(t *testing.T) {
	f, _, r := setup(t)
	f.failNext = true
	if c := post(t, r, msg("firing", alert("firing", "db-0"))); c != http.StatusBadGateway {
		t.Fatalf("want 502 so Alertmanager retries, got %d", c)
	}
	if r.failed.Load() != 1 {
		t.Error("failure not counted")
	}
}

func TestRejectsBadPayloads(t *testing.T) {
	_, _, r := setup(t)
	for name, b := range map[string][]byte{
		"not json":    []byte("{"),
		"wrong ver":   []byte(`{"version":"3","groupKey":"x","status":"firing"}`),
		"no groupKey": []byte(`{"version":"4","status":"firing"}`),
	} {
		if c := post(t, r, b); c != http.StatusBadRequest {
			t.Errorf("%s: got %d", name, c)
		}
	}
}

func TestBearerToken(t *testing.T) {
	f, _, r := setup(t)
	r.bearer = func() (string, error) { return "s3cret", nil }
	if c := post(t, r, msg("firing", alert("firing", "db-0"))); c != http.StatusUnauthorized {
		t.Fatalf("no token: %d", c)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(msg("firing", alert("firing", "db-0"))))
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 || len(f.issues) != 1 {
		t.Fatalf("with token: %d, issues %d", rec.Code, len(f.issues))
	}
}

func TestAlertTextCannotEscapeTheFence(t *testing.T) {
	a := alert("firing", "db-0")
	a.Annotations["description"] = "```\n@someone see https://evil.example.com #1\n```"
	m := Message{Status: "firing", GroupLabels: map[string]string{"alertname": "X"}, Alerts: []Alert{a}}
	b := body(&m, "id")
	// The description's own ``` must sit inside a longer fence.
	if !strings.Contains(b, "````text\n") {
		t.Fatalf("fence not lengthened:\n%s", b)
	}
	// Every line carrying the mention must be inside a fence.
	inside := false
	fenceLen := 0
	for _, line := range strings.Split(b, "\n") {
		trimmed := strings.TrimSuffix(line, "text")
		if strings.Trim(trimmed, "`") == "" && len(trimmed) >= 3 {
			switch {
			case !inside:
				inside, fenceLen = true, len(trimmed)
			case len(trimmed) >= fenceLen:
				inside = false
			}
			continue
		}
		if strings.Contains(line, "@someone") && !inside {
			t.Fatalf("mention outside a fence:\n%s", b)
		}
	}
}

func TestBodyIsCapped(t *testing.T) {
	var alerts []Alert
	for i := 0; i < 2000; i++ {
		a := alert("firing", fmt.Sprintf("pod-%04d", i))
		a.Annotations["description"] = strings.Repeat("x", 200)
		alerts = append(alerts, a)
	}
	m := Message{Status: "firing", GroupLabels: map[string]string{"alertname": "X"}, Alerts: alerts}
	b := body(&m, "id")
	if len(b) > 65536 {
		t.Fatalf("body %d chars, over GitHub's limit", len(b))
	}
	if !strings.Contains(b, "more alerts not shown") {
		t.Error("truncation not noted")
	}
}

func TestGroupIDStable(t *testing.T) {
	a := Message{Receiver: "github", GroupKey: "k"}
	b := Message{Receiver: "github", GroupKey: "k"}
	c := Message{Receiver: "other", GroupKey: "k"}
	if groupID(&a) != groupID(&b) || groupID(&a) == groupID(&c) {
		t.Fatal("groupID not stable or not receiver-scoped")
	}
}

func TestHugeHeaderStillUnderLimit(t *testing.T) {
	big := strings.Repeat("y", 100000)
	m := Message{
		Status:            "firing",
		GroupLabels:       map[string]string{"alertname": "X", "a": big},
		CommonAnnotations: map[string]string{"summary": big},
		ExternalURL:       big,
		Alerts:            []Alert{alert("firing", "db-0")},
	}
	if b := body(&m, "id"); len(b) > 65536 {
		t.Fatalf("body %d chars, over GitHub's limit", len(b))
	}
}

func TestQuotedMarkerDoesNotCaptureAnotherGroup(t *testing.T) {
	f, _, r := setup(t)
	// Another group's issue whose alert text quotes OUR group's marker.
	var m Message
	json.Unmarshal(msg("firing", alert("firing", "db-0")), &m)
	f.issues = append(f.issues, &fakeIssue{Number: 1, State: "open", Labels: []string{"alert"},
		Body: marker("other") + "\n\n```text\n" + marker(groupID(&m)) + "\n```\n"})
	post(t, r, msg("firing", alert("firing", "db-0")))
	if len(f.issues) != 2 {
		t.Fatalf("quoted marker matched: want a new issue, have %d", len(f.issues))
	}
	post(t, r, msg("resolved", alert("resolved", "db-0")))
	if f.issues[0].State != "open" || f.issues[1].State != "closed" {
		t.Fatalf("closed the wrong issue: %s %s", f.issues[0].State, f.issues[1].State)
	}
}

func TestBacktickFloodStaysBounded(t *testing.T) {
	ticks := strings.Repeat("`", 200000)
	m := Message{
		Status:            "firing",
		GroupLabels:       map[string]string{"alertname": "X", "a": ticks},
		CommonAnnotations: map[string]string{"summary": ticks, "description": ticks},
		ExternalURL:       ticks,
		Alerts:            []Alert{alert("firing", "db-0")},
	}
	b := body(&m, "id")
	if len(b) > 65536 {
		t.Fatalf("body %d chars, over GitHub's limit", len(b))
	}
	if !strings.Contains(b, "db-0") {
		t.Fatal("the alert itself was crowded out")
	}
}
