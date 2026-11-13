package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Message is the Alertmanager webhook payload, version 4.
// https://prometheus.io/docs/alerting/latest/configuration/#webhook_config
type Message struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts"`
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []Alert           `json:"alerts"`
}

// Alert is one alert inside a Message.
type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// maxBodyLen stays under GitHub's 65536-character limit on an issue body,
// with room for the truncation note. maxFieldLen caps any one fenced value,
// so a single huge label or annotation cannot use up the whole body before
// the first alert, or push it past the limit so the issue is never created.
const (
	maxBodyLen  = 60000
	maxFieldLen = 8000
)

// groupID identifies an alert group across notifications. Alertmanager's
// groupKey is stable for a group on one route, so its hash is what an open
// issue is matched on. The receiver name is folded in so that two routes
// pointing at the same repository cannot collide.
func groupID(m *Message) string {
	sum := sha256.Sum256([]byte(m.Receiver + "\x00" + m.GroupKey))
	return hex.EncodeToString(sum[:])[:20]
}

func marker(id string) string {
	return "<!-- alertmanager-group: " + id + " -->"
}

// title is built from the group labels only, so it does not change while
// the group fires and does not carry annotation text.
func title(m *Message) string {
	name := m.GroupLabels["alertname"]
	if name == "" {
		name = m.CommonLabels["alertname"]
	}
	if name == "" {
		name = "Alert group"
	}
	var rest []string
	for _, k := range sortedKeys(m.GroupLabels) {
		if k == "alertname" {
			continue
		}
		rest = append(rest, k+"="+m.GroupLabels[k])
	}
	t := name
	if len(rest) > 0 {
		t += " (" + strings.Join(rest, ", ") + ")"
	}
	if len(t) > 250 {
		t = t[:250]
	}
	return t
}

// body renders the issue body. Every value that came from an alert is
// written inside a fenced code block: alert text can be influenced by
// whatever the alert is about, and inside a fence GitHub does not render
// links, @mentions or #references.
func body(m *Message, id string) string {
	var b strings.Builder
	firing, resolved := 0, 0
	for _, a := range m.Alerts {
		if a.Status == "firing" {
			firing++
		} else {
			resolved++
		}
	}
	b.WriteString(marker(id) + "\n\n")
	fmt.Fprintf(&b, "**Status:** %s, %d firing, %d resolved", m.Status, firing, resolved)
	if m.TruncatedAlerts > 0 {
		fmt.Fprintf(&b, ", %d more not sent by Alertmanager", m.TruncatedAlerts)
	}
	b.WriteString("\n\n**Group labels**\n\n")
	b.WriteString(fence(kv(m.GroupLabels)))
	if len(m.CommonAnnotations) > 0 {
		b.WriteString("\n**Common annotations**\n\n")
		b.WriteString(fence(kv(m.CommonAnnotations)))
	}
	if m.ExternalURL != "" {
		b.WriteString("\n**Alertmanager**\n\n")
		b.WriteString(fence(m.ExternalURL))
	}

	alerts := append([]Alert(nil), m.Alerts...)
	sort.SliceStable(alerts, func(i, j int) bool {
		if alerts[i].Status != alerts[j].Status {
			return alerts[i].Status == "firing"
		}
		return alerts[i].StartsAt.Before(alerts[j].StartsAt)
	})
	for i, a := range alerts {
		var s strings.Builder
		fmt.Fprintf(&s, "\n### Alert %d: %s since %s\n\n", i+1, a.Status, a.StartsAt.UTC().Format(time.RFC3339))
		var detail strings.Builder
		detail.WriteString(kv(a.Labels))
		if len(a.Annotations) > 0 {
			detail.WriteString("\n" + kv(a.Annotations))
		}
		if a.GeneratorURL != "" {
			detail.WriteString("\nsource: " + a.GeneratorURL + "\n")
		}
		s.WriteString(fence(detail.String()))
		if b.Len()+s.Len() > maxBodyLen {
			fmt.Fprintf(&b, "\n_%d more alerts not shown: the issue body is limited in size._\n", len(alerts)-i)
			break
		}
		b.WriteString(s.String())
	}
	return b.String()
}

// With every fenced field bounded by fence(), the header (group labels,
// common annotations, URL) is at most about 3 x maxFieldLen, and the
// per-alert check above stops adding alerts at maxBodyLen, so the body stays
// under GitHub's 65,536-character limit. TestBodyIsCapped and
// TestHugeHeaderStillUnderLimit hold it to that.

func kv(m map[string]string) string {
	var b strings.Builder
	for _, k := range sortedKeys(m) {
		b.WriteString(k + ": " + m[k] + "\n")
	}
	return b.String()
}

// fence wraps s in a code fence longer than any backtick run inside it, so
// the content cannot close the fence early and escape into Markdown. Both
// the content and the fence are bounded: s is cut to maxFieldLen first, and
// any backtick run longer than maxTicks is shortened to maxTicks, so a field
// of nothing but backticks cannot make the fence itself enormous.
func fence(s string) string {
	if len(s) > maxFieldLen {
		s = strings.ToValidUTF8(s[:maxFieldLen], "") + "\n[truncated]\n"
	}
	s = longTicks.ReplaceAllString(s, strings.Repeat("`", maxTicks))
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", max(3, longest+1))
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return f + "text\n" + s + f + "\n"
}

const maxTicks = 10

var longTicks = regexp.MustCompile("`{" + strconv.Itoa(maxTicks+1) + ",}")

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
