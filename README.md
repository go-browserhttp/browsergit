# browsergit

The git layer shared by the go-tex, go-slides and notebook playgrounds: load a
project from a forge, and propose changes back as a **new branch plus a pull
request**. It is pure Go (CGO=0) and runs both natively and in the browser
(js/wasm).

```go
r, _ := forge.New(forge.Config{Kind: forge.GitHub, API: "https://api.github.com", Token: secret.New(tok)})
files, base, _ := r.Load(ctx, "owner/repo", "", "slides")          // default branch, one folder
files["deck.md"] = edited
res, _ := r.Propose(ctx, base, files, remote.Proposal{
    Branch: remote.BranchName("slides/", "Fix intro", time.Now()),
    Title:  "Fix intro",
})
// res.URL is the pull request
theirs, head, _ := r.Refresh(ctx, base)                              // nil when nothing moved
```

## Packages

| Package | What it is |
|---|---|
| `remote` | The `Remote` interface (`Load`, `Propose`, `Refresh`), `Base`, path/branch/repository validation, size limits, and `Git`, a `Remote` over git smart HTTP. |
| `forge` | `Remote` over the REST APIs of **GitHub**, **GitLab** and **Gitea/Forgejo**. |
| `secret` | `Token`: a credential that prints as `[redacted]` and refuses every encoder. |
| `browsergit` (root) | go-git v5 in memory (memfs), smart HTTP over the browser's Fetch: clone, status, commit, pull, push, snapshot. Moved here from go-tex's `playground/internal/browsergit`, history kept. |
| `gitrpc` | The JSON protocol between a page and a git web worker. Moved from go-tex, history kept. |

## Which way to reach a forge

| Forge | Use | Token |
|---|---|---|
| GitHub | `forge.GitHub` (REST; api.github.com answers CORS for any origin) | Fine-grained PAT: Metadata read, Contents read/write, Pull requests write. |
| GitLab | `forge.GitLab` (REST) | OAuth (PKCE) or PAT with `api` scope. |
| Gitea / Forgejo | `forge.Gitea` (REST) when the instance allows CORS; otherwise `remote.Git` through a CORS proxy | Access token with `write:repository`. Forgejo OAuth tokens have no scopes. |

`remote.Git` cannot open a pull request (smart HTTP has no such verb): it
pushes the new branch and returns the forge's compare page.

## Security

The token stays in the tab's memory and goes in one place: the Authorization
header of a request to the configured forge. Each point below has a test that
fails if it is broken.

- **Never written down.** `secret.Token` prints `[redacted]` under every `fmt`
  verb (also inside structs, also in unexported fields: the value lives in a
  registry, not in the struct) and makes `encoding/json`, `gob`, text and
  binary encoders fail. A draft, a version, an export or a share link that tried
  to carry one would fail to save rather than leak. `Forget` drops it at sign-out.
- **Never sent elsewhere.** Redirects are refused (natively by `CheckRedirect`;
  in the browser with Fetch `redirect: "error"`), URLs returned by a server
  (GitLab's `Link`) are never requested with the token, browser requests carry
  no cookies (`credentials: "omit"`), and API and web bases must be https
  (plain http only for a loopback host).
- **Never echoed.** Error messages keep only the server's `message` field,
  truncated and passed through `secret.Redact`, which also catches the token
  inside an echoed Basic header.
- **Never trusted back.** Paths from a server or a deck go through
  `remote.CheckPath` (no `..`, no absolute paths, no `.git`, no control
  characters); blob content must hash to its git id; response sizes, file counts
  and total bytes are bounded; the pull request link must be on the forge's web
  origin (no `javascript:`).
- **Never overwrites.** A proposal always creates a new branch: the name is
  checked against `git check-ref-format`, the loaded branch and the default
  branch are refused, the branch must not exist, and creation never forces. An
  optional prefix (`slides/`, or `slides/<user>/` on a server) is enforced.

A CORS proxy sees the Authorization header of every request through it. Its
operator can read tokens. Prefer the REST paths (no proxy) or a proxy you run
yourself.

Fuzzed: path, branch and repository validation, branch naming, tree and blob
decoding, redaction. `govulncheck` runs on every push and daily.

## Licence

BSD-3-Clause.
