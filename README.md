<p align="center">
  <a href="https://biga.n10nce.dev/">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="https://biga.n10nce.dev/brand/biga-logo-light.svg">
      <img src="https://biga.n10nce.dev/brand/biga-logo.svg" width="180" alt="biga">
    </picture>
  </a>
</p>

<h1 align="center">Share a folder from your terminal</h1>

<p align="center">
  The <code>biga</code> command watches a folder and streams every save to a live, read-only link.
</p>

<p align="center">
  <a href="https://github.com/biga-app/cli/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/biga-app/cli/ci.yml?branch=main&style=flat-square&labelColor=0c0d10&label=ci" alt="CI status"></a>
  <a href="https://github.com/biga-app/cli/releases/latest"><img src="https://img.shields.io/github/v/release/biga-app/cli?style=flat-square&labelColor=0c0d10&color=63e197&label=release" alt="Latest release"></a>
  <a href="go.mod"><img src="https://img.shields.io/github/go-mod/go-version/biga-app/cli?style=flat-square&labelColor=0c0d10&color=63e197" alt="Go version"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-63e197?style=flat-square&labelColor=0c0d10" alt="MIT license"></a>
</p>

<p align="center">
  <a href="https://biga.n10nce.dev/">Website</a> ·
  <a href="https://biga.n10nce.dev/docs">Documentation</a> ·
  <a href="https://github.com/biga-app/cli/releases">Releases</a> ·
  <a href="https://github.com/biga-app/cli/issues">Issues</a>
</p>

---

Run it in a project, send the link, keep working. Viewers browse the files, search, and step through checkpoints in their browser as you save. Nobody clones your repository or creates an account, and nobody can change your files.

## Get started

### Install

**macOS, Linux and FreeBSD**

```bash
curl -fsSL https://raw.githubusercontent.com/biga-app/cli/main/install.sh | sh
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/biga-app/cli/main/install.ps1 | iex
```

**With Go**

```bash
go install github.com/biga-app/cli/cmd/biga@latest
```

The install scripts check the download against the release's checksums, and against its Sigstore signature when `cosign` is installed. Set `BIGA_VERSION` to pin a version or `BIGA_INSTALL_DIR` to change where it goes (default `~/.local/bin`). See [installation](https://biga.n10nce.dev/docs/installation) for details.

### Share a project

From the folder you want to share:

```bash
biga start
```

biga uploads the folder, prints a link and keeps streaming every saved change until you stop it. Press <kbd>c</kbd> to copy the link, <kbd>o</kbd> to open it and <kbd>q</kbd> to stop. Running `biga start` in the same folder later rejoins the same session, so a link you already shared keeps working.

### Choose a server

By default the CLI talks to `http://localhost:3000`, a local copy of the [website](https://github.com/biga-app/website) and [server](https://github.com/biga-app/server). To share with people outside your machine, point it at a hosted instance:

```bash
BIGA_API_URL=https://biga.example.com BIGA_ACCESS_KEY=bgk_… biga start
```

To run your own, see [biga-app/docker](https://github.com/biga-app/docker).

## Commands

| Command | What it does |
| --- | --- |
| `biga start [dir...]` | Broadcast a folder and stream every save |
| `biga status [dir]` | Show the session for a folder, or a summary of all of them |
| `biga list` | List known sessions and whether they are live |
| `biga open [dir]` | Open a folder's session in the browser |
| `biga stop [dir]` | Stop a folder's session, or every session with `--all` |
| `biga extensions` | Create, list, or revoke checkpoint feed credentials |

## Configuration

| Variable | Purpose |
| --- | --- |
| `BIGA_API_URL` | Server to broadcast to. Same as `--api-url`. Default `http://localhost:3000`. |
| `BIGA_ACCESS_KEY` | Access key, for servers that require one |
| `BIGA_HOME` | Where local session state lives. Default `~/.biga`. |
| `BIGA_NO_UPDATE_CHECK` | Turn off the check for new releases |

Timing settings and the full reference are in the [CLI documentation](https://biga.n10nce.dev/docs/cli) and [configuration](https://biga.n10nce.dev/docs/configuration).

## Development

The CLI is a single Go module with no cgo. You need Go 1.26 or newer.

```bash
make test    # tests with the race detector
make lint    # golangci-lint
make build   # ./biga
```

[CONTRIBUTING.md](CONTRIBUTING.md) covers the project layout and what CI checks.

## Documentation

- [Quickstart](https://biga.n10nce.dev/docs/quickstart)
- [Installation](https://biga.n10nce.dev/docs/installation)
- [CLI reference](https://biga.n10nce.dev/docs/cli)
- [What gets shared](https://biga.n10nce.dev/docs/what-gets-shared)
- [Sessions](https://biga.n10nce.dev/docs/sessions)

## Repositories

| Repository | What it is |
| --- | --- |
| [cli](https://github.com/biga-app/cli) | The `biga` command. Watches a folder and streams every save. |
| [server](https://github.com/biga-app/server) | The Go API. Stores sessions and streams updates to viewers. |
| [website](https://github.com/biga-app/website) | The site, the live viewer and the documentation. |
| [docker](https://github.com/biga-app/docker) | Self-hosting: the whole stack with Docker Compose. |

## Contributing

Bug reports, focused fixes and pull requests are welcome. Read the [contributing guide](CONTRIBUTING.md) before you start, and report vulnerabilities privately as described in the [security policy](https://github.com/biga-app/cli/security/policy), not in a public issue.

## License

biga is [MIT licensed](LICENSE).

---

<p align="center">
  <sub><a href="https://biga.n10nce.dev/">biga</a> · Made for people who teach, pair and build in public.</sub>
</p>
