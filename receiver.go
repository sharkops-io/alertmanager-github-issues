package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Issues is what the receiver needs from GitHub.
type Issues interface {
	OpenIssues(ctx context.Context, label string) ([]Issue, error)
	CreateIssue(ctx context.Context, title, body string, labels []string) (int, error)
	UpdateIssue(ctx context.Context, number int, fields map[string]any) error
	Comment(ctx context.Context, number int, text string) error
}

// Receiver turns Alertmanager notifications into GitHub issues: one open
// issue per alert group while it fires, closed when the group resolves.
// It keeps no state of its own; the open issues are the state.
type Receiver struct {
	gh            Issues
	label         string
	labelFromKeys []string // alert label names copied onto the issue as "name:value" labels
	bearer        func() (string, error)
	log           *slog.Logger
	now           func() time.Time

	// One notification at a time. Volume is a handful per group_interval,
	// and serialising rules out two concurrent notifications for one group
	// each finding no open issue and both creating one.
	mu sync.Mutex

	opened, closed, updated, failed atomic.Int64
}

const maxPayload = 4 << 20

func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r.bearer != nil {
		want, err := r.bearer()
		if err != nil {
			r.log.Error("reading bearer token", "err", err)
			http.Error(w, "server misconfigured", http.StatusInternalServerError)
			return
		}
		got := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	data, err := io.ReadAll(io.LimitReader(req.Body, maxPayload+1))
	if err != nil || len(data) > maxPayload {
		http.Error(w, "payload unreadable or too large", http.StatusBadRequest)
		return
	}
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if m.Version != "4" || m.GroupKey == "" {
		http.Error(w, "not an Alertmanager v4 webhook payload", http.StatusBadRequest)
		return
	}
	if err := r.Handle(req.Context(), &m); err != nil {
		r.failed.Add(1)
		r.log.Error("handling notification", "group", title(&m), "status", m.Status, "err", err)
		// A 5xx makes Alertmanager retry and count the failure in
		// alertmanager_notifications_failed_total, which is the alert
		// for this receiver being broken.
		http.Error(w, "github request failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// Handle applies one notification.
func (r *Receiver) Handle(ctx context.Context, m *Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	id := groupID(m)
	existing, err := r.find(ctx, id)
	if err != nil {
		return err
	}
	switch m.Status {
	case "firing":
		b := body(m, id)
		if existing == nil {
			n, err := r.gh.CreateIssue(ctx, title(m), b, r.labels(m))
			if err != nil {
				return err
			}
			r.opened.Add(1)
			r.log.Info("opened issue", "issue", n, "group", title(m))
			return nil
		}
		if existing.Body == b {
			return nil
		}
		if err := r.gh.UpdateIssue(ctx, existing.Number, map[string]any{"body": b}); err != nil {
			return err
		}
		r.updated.Add(1)
		return nil
	case "resolved":
		if existing == nil {
			return nil
		}
		msg := fmt.Sprintf("Resolved: every alert in this group resolved (notified %s).", r.now().UTC().Format(time.RFC3339))
		if err := r.gh.Comment(ctx, existing.Number, msg); err != nil {
			return err
		}
		if err := r.gh.UpdateIssue(ctx, existing.Number, map[string]any{
			"body": body(m, id), "state": "closed", "state_reason": "completed",
		}); err != nil {
			return err
		}
		r.closed.Add(1)
		r.log.Info("closed issue", "issue", existing.Number, "group", title(m))
		return nil
	default:
		return fmt.Errorf("unknown status %q", m.Status)
	}
}

func (r *Receiver) find(ctx context.Context, id string) (*Issue, error) {
	open, err := r.gh.OpenIssues(ctx, r.label)
	if err != nil {
		return nil, err
	}
	// The marker must OPEN the body, as body() writes it. Anywhere else it
	// could be alert text quoting another group's marker, which would
	// redirect this group's updates and closure to that group's issue.
	mk := marker(id) + "\n"
	for i := range open {
		if strings.HasPrefix(open[i].Body, mk) {
			return &open[i], nil
		}
	}
	return nil, nil
}

func (r *Receiver) labels(m *Message) []string {
	out := []string{r.label}
	for _, k := range r.labelFromKeys {
		if v := m.CommonLabels[k]; v != "" {
			l := k + ":" + v
			if len(l) > 50 { // GitHub's label name limit
				l = l[:50]
			}
			out = append(out, l)
		}
	}
	return out
}

func (r *Receiver) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintln(w, "# HELP alertmanager_github_issues_total Issues changed, by action.")
	fmt.Fprintln(w, "# TYPE alertmanager_github_issues_total counter")
	fmt.Fprintf(w, "alertmanager_github_issues_total{action=\"opened\"} %d\n", r.opened.Load())
	fmt.Fprintf(w, "alertmanager_github_issues_total{action=\"updated\"} %d\n", r.updated.Load())
	fmt.Fprintf(w, "alertmanager_github_issues_total{action=\"closed\"} %d\n", r.closed.Load())
	fmt.Fprintln(w, "# HELP alertmanager_github_issues_failures_total Notifications that could not be applied.")
	fmt.Fprintln(w, "# TYPE alertmanager_github_issues_failures_total counter")
	fmt.Fprintf(w, "alertmanager_github_issues_failures_total %d\n", r.failed.Load())
}
