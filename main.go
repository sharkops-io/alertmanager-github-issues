// Command alertmanager-github-issues is an Alertmanager webhook receiver
// that keeps one GitHub issue open per firing alert group and closes it
// when the group resolves.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "alertmanager-github-issues:", err)
		os.Exit(1)
	}
}

// env lets every flag also be set as AGI_<NAME>, upper-cased with dashes
// as underscores; an explicit flag wins.
func env(name, def string) string {
	if v, ok := os.LookupEnv("AGI_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))); ok {
		return v
	}
	return def
}

func run(args []string) error {
	fs := flag.NewFlagSet("alertmanager-github-issues", flag.ContinueOnError)
	var (
		addr        = fs.String("listen", env("listen", ":8080"), "listen address")
		repo        = fs.String("repo", env("repo", ""), "target repository, owner/name (required)")
		apiURL      = fs.String("api-url", env("api-url", "https://api.github.com"), "GitHub API base URL")
		label       = fs.String("label", env("label", "alert"), "label put on every issue and used to find open ones")
		labelKeys   = fs.String("label-from", env("label-from", "severity"), "comma-separated alert label names copied onto the issue as name:value labels")
		appID       = fs.String("app-id", env("app-id", ""), "GitHub App ID or client ID")
		install     = fs.String("installation-id", env("installation-id", ""), "GitHub App installation ID")
		keyFile     = fs.String("private-key-file", env("private-key-file", ""), "GitHub App private key (PEM)")
		tokenFile   = fs.String("token-file", env("token-file", ""), "file holding a token, instead of a GitHub App")
		bearerFile  = fs.String("bearer-token-file", env("bearer-token-file", ""), "if set, the webhook requires Authorization: Bearer <contents of this file>")
		logJSON     = fs.Bool("log-json", env("log-json", "true") == "true", "log as JSON")
		httpTimeout = fs.Duration("github-timeout", 20*time.Second, "timeout for each GitHub API request")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	var log *slog.Logger
	if *logJSON {
		log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	} else {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}

	owner, name, ok := strings.Cut(*repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return errors.New("-repo must be owner/name")
	}
	if strings.Contains(*label, ",") || *label == "" {
		return errors.New("-label must be non-empty and contain no comma")
	}
	client := &http.Client{Timeout: *httpTimeout}
	api := strings.TrimRight(*apiURL, "/")

	var tokens TokenSource
	switch {
	case *tokenFile != "" && (*appID != "" || *keyFile != ""):
		return errors.New("use either -token-file or the GitHub App flags, not both")
	case *tokenFile != "":
		tokens = fileToken{path: *tokenFile}
	case *appID != "" && *install != "" && *keyFile != "":
		tokens = &appToken{apiURL: api, appID: *appID, installationID: *install, keyPath: *keyFile, repo: name, client: client, now: time.Now}
	default:
		return errors.New("set -app-id, -installation-id and -private-key-file, or -token-file")
	}

	r := &Receiver{
		gh:    &GitHub{apiURL: api, owner: owner, repo: name, tokens: tokens, client: client},
		label: *label,
		log:   log,
		now:   time.Now,
	}
	for _, k := range strings.Split(*labelKeys, ",") {
		if k = strings.TrimSpace(k); k != "" {
			r.labelFromKeys = append(r.labelFromKeys, k)
		}
	}
	if *bearerFile != "" {
		r.bearer = func() (string, error) { return fileToken{path: *bearerFile}.Token(context.Background()) }
	}

	mux := http.NewServeMux()
	mux.Handle("POST /webhook", r)
	mux.HandleFunc("GET /metrics", r.metrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", *addr, "repo", *repo)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shut)
	}
}
