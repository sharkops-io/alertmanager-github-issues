# alertmanager-github-issues

An Alertmanager webhook receiver that keeps **one GitHub issue open per firing
alert group**, and closes it when the group resolves.

- **Open issues are what is firing now; closed issues are the history.** A group
  that fires again after it resolved gets a new issue, so "tell me only about new
  things" is just "watch for opened issues".
- **Stateless.** The open issues are the state. Each issue carries a hidden marker
  with a hash of Alertmanager's group key, which is how a later notification for
  the same group finds it. Restarting the receiver loses nothing.
- **No dependencies** beyond the Go standard library.

## What it does with each notification

| Alertmanager sends | Open issue for the group? | Result |
|---|---|---|
| `firing` | no | creates an issue |
| `firing` | yes, same alerts | nothing |
| `firing` | yes, alerts changed | rewrites the issue body |
| `resolved` | yes | comments, rewrites the body, closes it as completed |
| `resolved` | no | nothing |

The title is the group's `alertname` plus its other group labels, for example
`KubePodCrashLooping (namespace=web)`. It does not change while the group fires.

Each issue gets the `-label` label (default `alert`) and one `name:value` label for
each alert label named in `-label-from` (default `severity`), taken from the
group's common labels.

If someone closes an issue by hand while its group is still firing, the next
notification for that group (a repeat, or a change to its alerts) opens a new one.

## Alert text is untrusted

Labels and annotations often carry text from whatever the alert is about: a pod
name, a log line, a URL. Every such value is written **inside a fenced code
block**, and the fence is made longer than any run of backticks in the text, so
the content cannot close it. Inside a fence GitHub renders no links, `@mentions`
or `#references`. If something downstream reads these issues (a person, a bot, an
LLM), treat the fenced text as data, not instructions.

The body is capped below GitHub's 65,536-character limit; alerts that do not fit
are counted, not shown.

## Authentication

A **GitHub App** is the intended setup. Give it **Issues: read and write** (Metadata
read is implied), install it on the one repository the issues go to, and pass:

| flag | env | |
|---|---|---|
| `-app-id` | `AGI_APP_ID` | App ID or client ID |
| `-installation-id` | `AGI_INSTALLATION_ID` | the installation on your account or org |
| `-private-key-file` | `AGI_PRIVATE_KEY_FILE` | the App's private key, PEM (PKCS#1 or PKCS#8) |

Each installation token is requested for **only the `-repo` repository and only
the issues permission**, whatever else the installation could reach, and is cached
until five minutes before it expires. The key file is re-read whenever a token is
minted, so a rotated key needs no restart.

For a quick test, `-token-file` takes a file holding any token with issue write
access instead. It is re-read on every request.

## Configuration

Every flag can also be set as an environment variable: `AGI_` plus the flag name in
upper case with dashes as underscores. A flag on the command line wins.

| flag | default | |
|---|---|---|
| `-repo` | (required) | `owner/name` the issues go to |
| `-listen` | `:8080` | listen address |
| `-label` | `alert` | label on every issue; open issues are found by it |
| `-label-from` | `severity` | comma-separated alert label names copied onto the issue |
| `-bearer-token-file` | | if set, `POST /webhook` requires `Authorization: Bearer <file contents>` |
| `-api-url` | `https://api.github.com` | for GitHub Enterprise Server, `https://HOST/api/v3` |
| `-github-timeout` | `20s` | per GitHub API request |
| `-log-json` | `true` | JSON logs to stderr; `false` for text |

## Endpoints

| | |
|---|---|
| `POST /webhook` | Alertmanager webhook, payload version 4 |
| `GET /healthz` | `ok`; does not call GitHub |
| `GET /metrics` | `alertmanager_github_issues_total{action="opened\|updated\|closed"}`, `alertmanager_github_issues_failures_total` |

When GitHub fails, the receiver answers `502`, so Alertmanager retries and counts
the failure in `alertmanager_notifications_failed_total{integration="webhook"}`.
An alert on that metric covers this receiver being broken.

## Alertmanager configuration

Send everything to the receiver **and** keep your existing notifications, with a
`continue: true` route placed before your catch-all:

```yaml
route:
  receiver: chat
  routes:
    - receiver: github-issues
      continue: true
    - receiver: chat
receivers:
  - name: github-issues
    webhook_configs:
      - url: http://alertmanager-github-issues.monitoring.svc:8080/webhook
        send_resolved: true   # required, or issues never close
        max_alerts: 0
  - name: chat
    # ...
```

`send_resolved: true` is what closes issues. With `group_by` on, say,
`[namespace, alertname]`, one issue covers every pod in a namespace hitting the
same alert.

## Run

```sh
docker run -p 8080:8080 \
  -v /path/to/key.pem:/key.pem:ro \
  ghcr.io/sharkops-io/alertmanager-github-issues:latest \
  -repo example-org/alerts -app-id 12345 -installation-id 678 -private-key-file /key.pem
```

The image is distroless and runs as non-root.

## Development

```sh
go test ./... -race
```

The tests run against an in-memory fake of the issues API, including pagination
(an open issue past the first page of 100 must still be found).

## License

Apache-2.0.
