# barge

**English** | [简体中文](README.zh.md)

`barge` is a command-line tool that pulls container images from any registry (Docker Hub, GHCR, GCR, Quay, private registries, ...) and packs them into a `docker load`-compatible tar file. **No Docker daemon required.**

Typical use cases: moving images into an air-gapped network, or archiving a specific image version to disk.

## Features

- Cross-platform: macOS / Windows / Linux, amd64 / arm64 — single static binary
- Proxy support: `--proxy` flag or `HTTPS_PROXY` environment variable
- Resumable downloads: HTTP `Range` + sha256 verification — interrupted pulls continue where they left off
- Automatic retries: exponential backoff on network errors / 5xx / EOF (configurable)
- Layer deduplication: content-addressable cache — layers shared across images are downloaded once
- Concurrent downloads: multiple layers in parallel
- Authentication: `--username/--password`, `--password-stdin`, and automatic use of `~/.docker/config.json`
- Multi-arch images: selectable via `--platform` (defaults to host arch)
- Zero third-party dependencies: Go standard library only

## Install

### Download a prebuilt binary

Grab the archive for your platform from the [Releases](../../releases) page:

- `barge_vX.Y.Z_darwin_amd64.tar.gz` / `darwin_arm64.tar.gz`
- `barge_vX.Y.Z_windows_amd64.zip` / `windows_arm64.zip`
- `barge_vX.Y.Z_linux_amd64.tar.gz` / `linux_arm64.tar.gz`

Extract and drop the binary onto your `PATH`.

### Build from source (Docker or Go)

Cross-compile with Docker:

```bash
docker build -t barge .
docker run --rm -v "$PWD:/data" barge pull alpine:3.20
```

Or build with Go directly:

```bash
go build -trimpath -ldflags "-s -w" -o barge .
```

## Usage

### Basic

```bash
barge pull nginx:1.25
# => writes nginx_1.25.tar

docker load -i nginx_1.25.tar
```

### Output path and platform

```bash
barge pull -o /tmp/img.tar -p linux/arm64 alpine:3.20
```

### Proxy

```bash
barge pull -x http://127.0.0.1:7890 nginx:latest
# -x is the short form of --proxy

# Or via environment variable:
export HTTPS_PROXY=http://127.0.0.1:7890
barge pull nginx:latest
```

### Private images (GHCR / private registries)

**Option A: pass credentials**

```bash
# Read the token from stdin — stays out of shell history
echo $GITHUB_TOKEN | barge pull --password-stdin -u YOUR_USERNAME \
    ghcr.io/org/private-image:tag

# Or pass directly (not recommended)
barge pull -u YOUR_USERNAME --password $GITHUB_TOKEN \
    ghcr.io/org/private-image:tag
```

**Option B: reuse `docker login` credentials**

If you have already run `docker login ghcr.io`, `barge` will auto-discover credentials from `~/.docker/config.json`:

```bash
docker login ghcr.io                    # one-time login
barge pull ghcr.io/org/private-image:tag  # authenticated automatically
```

**Option C: point at a specific docker config**

```bash
barge pull --docker-config /path/to/config.json ghcr.io/org/image:tag
```

### Pull by digest

```bash
barge pull alpine@sha256:c5b1261d6d3e43071626931fc004f10189d3b5df5e3b85df68e6b0dd1b1a8f5e
```

### Resume and cache

If a download is interrupted (Ctrl+C, flaky network), just **run the same command again** — finished blobs are reused by sha256, and unfinished ones resume from the byte offset via HTTP `Range`:

```bash
barge pull very-large-image:tag
# ...interrupted...
barge pull very-large-image:tag   # continues automatically
```

All blobs are stored content-addressably in `~/.barge/blobs/` under their sha256 names. The same layer across different images is downloaded only once; unfinished blobs carry a `.part` suffix.

Override the data root with the `BARGE_HOME` environment variable:

```bash
export BARGE_HOME=/mnt/bigdisk/barge
barge pull nginx:1.25
# => blobs land in /mnt/bigdisk/barge/blobs/
```

### Retries

Transient failures (network errors, HTTP 5xx, unexpected EOF) are retried automatically with exponential backoff (1s → 2s → 4s → …, capped at 30s). The on-disk `.part` file preserves progress, so retries never restart from zero. Non-retryable errors (4xx, sha256 mismatch, context cancellation) fail fast.

`--retries N` caps **consecutive failures without progress**, not total attempts. For blob downloads, the budget resets whenever an attempt grows the `.part` file — a flaky connection that drops every 50MB but keeps advancing will finish, while a fully stalled one gives up after N in a row.

```bash
barge pull --retries 5 big-image:tag   # up to 5 consecutive stalled failures
barge pull --retries 0 big-image:tag   # disable retries entirely
```

### Inspect and clean the cache

```bash
barge status              # show blob count, .part count, total size
barge clean               # remove .part files only
barge clean --all         # also purge cached blobs
barge clean -a            # same as above
```

## Commands and flags

### Top-level commands

| Command | Description |
|---|---|
| `barge pull <image> [flags]` | Pull an image and pack it into a tar |
| `barge status` | Show cache status |
| `barge clean [--all]` | Remove `.part` files; `--all` also purges cached blobs |
| `barge --version` | Print version |

### `pull` flags

| Flag | Short | Description |
|---|---|---|
| `--output` | `-o` | Output tar path (default `<repo>_<tag>.tar`) |
| `--platform` | `-p` | Target platform `os/arch[/variant]` (defaults to host arch) |
| `--proxy` | `-x` | HTTP/HTTPS proxy URL (falls back to `HTTPS_PROXY`) |
| `--concurrency` | `-c` | Number of layers downloaded in parallel (default 3) |
| `--retries` | `-r` | Max consecutive failures without progress per blob/manifest (default 3) |
| `--username` | `-u` | Registry username |
| `--password` |   | Password / token (prefer `--password-stdin`) |
| `--password-stdin` |   | Read password / token from stdin |
| `--docker-config` |   | Path to `docker config.json` |

### Environment variables

| Variable | Description |
|---|---|
| `BARGE_HOME` | Data root (default `~/.barge`). Blob cache lives under `$BARGE_HOME/blobs/`. |
| `HTTPS_PROXY` / `HTTP_PROXY` | Proxy (used when `--proxy` is not set) |
| `DOCKER_CONFIG` | Docker config directory (uses `$DOCKER_CONFIG/config.json` when `--docker-config` is not set) |

## Reference format

```
nginx                           -> registry-1.docker.io/library/nginx:latest
nginx:1.25                      -> registry-1.docker.io/library/nginx:1.25
alice/app:1.0                   -> registry-1.docker.io/alice/app:1.0
ghcr.io/owner/repo:tag          -> ghcr.io/owner/repo:tag
gcr.io/project/img@sha256:abc   -> gcr.io/project/img@sha256:abc
localhost:5000/team/svc:dev     -> localhost:5000/team/svc:dev
```

## How it works

1. Parse the image reference — registry, repository, tag or digest
2. Send `GET /v2/`; on `401`, negotiate Bearer token or Basic auth from the `Www-Authenticate` header
3. Fetch the manifest; if it is a manifest list / OCI index, pick a child by `--platform` and refetch
4. `GET /v2/<repo>/blobs/<digest>` for config and each layer with `Range` for resume; verify sha256 and atomically rename on completion
5. Assemble `manifest.json + <config>.json + <layer>.tar.gz` into a `docker load`-compatible tar

Transient failures in steps 2–4 are retried with exponential backoff.

## Build

No third-party dependencies — Go 1.22+ is all you need:

```bash
go build -trimpath -ldflags "-s -w -X main.version=dev" -o barge .
```

Cross-compile example (produce a Windows arm64 binary from any host):

```bash
GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go build -o barge.exe .
```

## Releasing

Pushing a `v*` tag triggers a GitHub Actions workflow that builds six-platform binaries (macOS / Windows / Linux × amd64 / arm64), archives them, and publishes to GitHub Release along with `checksums.txt`.

```bash
git tag v0.1.0
git push origin v0.1.0
```

## License

MIT
