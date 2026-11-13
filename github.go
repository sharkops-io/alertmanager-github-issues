package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// TokenSource returns a token for the GitHub API.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// fileToken reads a static token (a personal access token, say) from a
// file on every call, so a rotated Secret is picked up without a restart.
type fileToken struct{ path string }

func (f fileToken) Token(context.Context) (string, error) {
	b, err := os.ReadFile(f.path)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", fmt.Errorf("%s is empty", f.path)
	}
	return t, nil
}

// appToken mints GitHub App installation tokens, narrowed to one repository
// and to the issues permission, and caches each until shortly before it
// expires.
type appToken struct {
	apiURL         string
	appID          string
	installationID string
	keyPath        string
	repo           string // name only, without the owner
	client         *http.Client
	now            func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

func (a *appToken) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && a.now().Before(a.expires.Add(-5*time.Minute)) {
		return a.token, nil
	}
	jwt, err := a.jwt()
	if err != nil {
		return "", err
	}
	req := map[string]any{
		"repositories": []string{a.repo},
		"permissions":  map[string]string{"issues": "write", "metadata": "read"},
	}
	var resp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	u := fmt.Sprintf("%s/app/installations/%s/access_tokens", a.apiURL, a.installationID)
	if _, err := doJSON(ctx, a.client, http.MethodPost, u, jwt, req, &resp); err != nil {
		return "", fmt.Errorf("installation token: %w", err)
	}
	a.token, a.expires = resp.Token, resp.ExpiresAt
	return a.token, nil
}

// jwt signs the short-lived app JWT (RS256). The key file is read each time
// so a rotated key needs no restart.
func (a *appToken) jwt() (string, error) {
	raw, err := os.ReadFile(a.keyPath)
	if err != nil {
		return "", err
	}
	key, err := parseRSAKey(raw)
	if err != nil {
		return "", err
	}
	now := a.now()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		// Backdated a minute for clock drift, as GitHub's docs suggest.
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.appID,
	})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

func parseRSAKey(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("private key: no PEM block found")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("private key: neither PKCS#1 nor PKCS#8")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key: not an RSA key")
	}
	return rk, nil
}

// Issue is the subset of a GitHub issue this program reads.
type Issue struct {
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	PullRequest json.RawMessage `json:"pull_request,omitempty"`
}

// GitHub is a minimal issues client for one repository.
type GitHub struct {
	apiURL string
	owner  string
	repo   string
	tokens TokenSource
	client *http.Client
}

// OpenIssues lists every open issue carrying label, following pagination
// to the end: a single page silently stops at 100.
func (g *GitHub) OpenIssues(ctx context.Context, label string) ([]Issue, error) {
	tok, err := g.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	var all []Issue
	u := fmt.Sprintf("%s/repos/%s/%s/issues?state=open&per_page=100&labels=%s", g.apiURL, g.owner, g.repo, url.QueryEscape(label))
	for u != "" {
		var page []Issue
		h, err := doJSON(ctx, g.client, http.MethodGet, u, "token "+tok, nil, &page)
		if err != nil {
			return nil, err
		}
		for _, is := range page {
			if len(is.PullRequest) == 0 {
				all = append(all, is)
			}
		}
		u = nextLink(h.Get("Link"))
	}
	return all, nil
}

func (g *GitHub) CreateIssue(ctx context.Context, title, body string, labels []string) (int, error) {
	tok, err := g.tokens.Token(ctx)
	if err != nil {
		return 0, err
	}
	var out Issue
	u := fmt.Sprintf("%s/repos/%s/%s/issues", g.apiURL, g.owner, g.repo)
	_, err = doJSON(ctx, g.client, http.MethodPost, u, "token "+tok,
		map[string]any{"title": title, "body": body, "labels": labels}, &out)
	return out.Number, err
}

func (g *GitHub) UpdateIssue(ctx context.Context, number int, fields map[string]any) error {
	tok, err := g.tokens.Token(ctx)
	if err != nil {
		return err
	}
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d", g.apiURL, g.owner, g.repo, number)
	_, err = doJSON(ctx, g.client, http.MethodPatch, u, "token "+tok, fields, nil)
	return err
}

func (g *GitHub) Comment(ctx context.Context, number int, text string) error {
	tok, err := g.tokens.Token(ctx)
	if err != nil {
		return err
	}
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", g.apiURL, g.owner, g.repo, number)
	_, err = doJSON(ctx, g.client, http.MethodPost, u, "token "+tok, map[string]any{"body": text}, nil)
	return err
}

// doJSON sends one request and decodes a JSON response. auth is either
// "token <t>" (sent as is) or a bare app JWT (sent as "Bearer <jwt>").
func doJSON(ctx context.Context, c *http.Client, method, u, auth string, in, out any) (http.Header, error) {
	var rd io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(auth, "token ") {
		req.Header.Set("Authorization", auth)
	} else {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "alertmanager-github-issues")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("%s %s: %s: %s", method, redactURL(u), resp.Status, msg)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, fmt.Errorf("%s %s: decode: %w", method, redactURL(u), err)
		}
	}
	return resp.Header, nil
}

var linkNext = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func nextLink(h string) string {
	if m := linkNext.FindStringSubmatch(h); m != nil {
		return m[1]
	}
	return ""
}

func redactURL(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}
