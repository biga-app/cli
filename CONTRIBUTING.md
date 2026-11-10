# Contributing

Thanks for helping with biga. The CLI is a single Go module with no cgo, so a
Go toolchain is all you need.

## Build and test

```sh
make test   # go test -race ./...
make lint   # golangci-lint run ./...
make build  # ./biga
```

Run it against a local copy of [the server](https://github.com/biga-app/website)
(`http://localhost:3000` is the default), or point it elsewhere with
`BIGA_API_URL`.

CI runs the tests on Linux, macOS and Windows, each on x64 and arm64, and lints
the code for each OS. It also runs `govulncheck`, lints and security-audits the
workflows (actionlint, zizmor), and does a full release dry run, whose builds
are attached to the run and installed with `install.sh` / `install.ps1` on
every one of those platforms.

## Project layout

| Path | What lives there |
| --- | --- |
| `cmd/biga` | The entry point. |
| `internal/cli` | Commands and flags. |
| `internal/broadcast` | Start, rejoin, flush, heartbeat and shutdown. |
| `internal/watch` | The file watcher and the batcher that coalesces changes. |
| `internal/snapshot`, `internal/ignore` | Reading a tree, and what to leave out of it. |
| `internal/api` | The HTTP client: retries, gzip, the wire types. |
| `internal/session`, `internal/workspace` | The local registry, and `biga.json`. |
| `internal/ui`, `internal/human` | The live panel, prompts, and number formatting. |

## Releasing

1. Set the version in `internal/version/version.go` and merge it to `main`.
2. Tag the commit and push the tag:

   ```sh
   git tag -s v1.0.0 -m "biga 1.0.0"
   git push origin v1.0.0
   ```

The release workflow reruns CI, then GoReleaser builds every target, signs
`checksums.txt`, attests build provenance, and publishes the GitHub release.
Builds are reproducible: rebuilding a tag produces byte-identical files. Try
the whole release locally without publishing anything with `make snapshot`.

To bake a hosted server into release binaries, set the `DefaultAPIURL` ldflag
in `.goreleaser.yaml`.

### Signing macOS binaries

macOS binaries can be signed and notarized so Gatekeeper opens them without a
warning. Add these secrets to the `release` environment (Settings →
Environments) to turn that on; without them it is skipped:

| Secret | Value |
| --- | --- |
| `MACOS_SIGN_P12` | A *Developer ID Application* certificate exported as `.p12`, base64-encoded (`base64 -w0 < cert.p12`) |
| `MACOS_SIGN_PASSWORD` | The password set when exporting it |
| `MACOS_NOTARY_ISSUER_ID` | The issuer ID of an App Store Connect API key |
| `MACOS_NOTARY_KEY_ID` | That key's ID |
| `MACOS_NOTARY_KEY` | The key's `.p8` file, base64-encoded |
