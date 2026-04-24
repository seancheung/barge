package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/theoxuanx/barge/internal/registry"
)

type urlsOptions struct {
	platform      string
	proxyURL      string
	username      string
	password      string
	passwordStdin bool
	dockerConfig  string
	format        string
}

func runURLs(args []string) error {
	var opts urlsOptions
	defaultPlatform := "linux/" + runtime.GOARCH

	fs := flag.NewFlagSet("urls", flag.ExitOnError)
	fs.StringVar(&opts.platform, "platform", defaultPlatform, "target platform os/arch[/variant]")
	fs.StringVar(&opts.platform, "p", defaultPlatform, "alias of --platform")
	fs.StringVar(&opts.proxyURL, "proxy", "", "HTTP/HTTPS proxy URL (falls back to HTTPS_PROXY env)")
	fs.StringVar(&opts.proxyURL, "x", "", "alias of --proxy")
	fs.StringVar(&opts.username, "username", "", "registry username")
	fs.StringVar(&opts.username, "u", "", "alias of --username")
	fs.StringVar(&opts.password, "password", "", "registry password or token (prefer --password-stdin)")
	fs.BoolVar(&opts.passwordStdin, "password-stdin", false, "read password/token from stdin")
	fs.StringVar(&opts.dockerConfig, "docker-config", "", "path to docker config.json")
	fs.StringVar(&opts.format, "format", "text", "output format: text | aria2")
	fs.StringVar(&opts.format, "f", "text", "alias of --format")

	fs.Usage = func() {
		name := filepath.Base(os.Args[0])
		fmt.Fprintf(os.Stderr, "Usage: %s urls <image> [flags]\n\n", name)
		fmt.Fprintln(os.Stderr, "List every blob in the image along with its download URL, target cache")
		fmt.Fprintln(os.Stderr, "path, and current on-disk progress. Feed the output to an external downloader")
		fmt.Fprintln(os.Stderr, "(IDM, aria2, wget, ...), place each file at the listed path, then run")
		fmt.Fprintln(os.Stderr, "'barge pull' to verify and pack the image.\n")
		fmt.Fprintln(os.Stderr, "Examples:")
		fmt.Fprintf(os.Stderr, "  %s urls ghcr.io/owner/repo:tag\n", name)
		fmt.Fprintf(os.Stderr, "  %s urls -f aria2 ghcr.io/owner/repo:tag > urls.txt\n", name)
		fmt.Fprintf(os.Stderr, "  %s urls -x http://127.0.0.1:7890 -p linux/arm64 owner/repo:tag\n\n", name)
		fmt.Fprintln(os.Stderr, "Flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("exactly one image reference is required")
	}
	return doURLs(fs.Arg(0), opts)
}

func doURLs(image string, opts urlsOptions) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	ref, err := registry.ParseReference(image)
	if err != nil {
		return fmt.Errorf("parse image reference: %w", err)
	}

	auth, err := buildAuth(ref, opts.dockerConfig, opts.username, opts.password, opts.passwordStdin)
	if err != nil {
		return err
	}

	client, err := registry.NewClient(opts.proxyURL, auth)
	if err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "fetching manifest...")
	manifest, _, _, err := client.ResolveManifest(ctx, ref, opts.platform)
	if err != nil {
		return fmt.Errorf("fetch manifest: %w", err)
	}

	dir, err := blobsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	authHeader := client.AuthHeaderFor(ref.Registry, ref.Repository)

	entries := make([]blobEntry, 0, 1+len(manifest.Layers))
	entries = append(entries, inspectBlob("config", manifest.Config.Digest, manifest.Config.Size, ref, dir))
	for _, l := range manifest.Layers {
		entries = append(entries, inspectBlob("layer", l.Digest, l.Size, ref, dir))
	}

	switch opts.format {
	case "aria2":
		return printAria2(os.Stdout, entries, authHeader)
	case "text", "":
		return printText(os.Stdout, entries, ref, opts.platform, dir, authHeader)
	default:
		return fmt.Errorf("unknown format %q (valid: text, aria2)", opts.format)
	}
}

type blobEntry struct {
	kind     string // "config" or "layer"
	digest   string // "sha256:..."
	size     int64
	url      string
	savePath string // absolute final cache path (no .part suffix)
	partPath string // savePath + ".part"
	status   string // "done" | "partial" | "missing"
	have     int64  // bytes already on disk (0 for missing)
}

func inspectBlob(kind, digest string, size int64, ref registry.Reference, dir string) blobEntry {
	final := filepath.Join(dir, digestHex(digest))
	e := blobEntry{
		kind:     kind,
		digest:   digest,
		size:     size,
		url:      fmt.Sprintf("https://%s/v2/%s/blobs/%s", ref.Registry, ref.Repository, digest),
		savePath: final,
		partPath: final + ".part",
	}
	if fi, err := os.Stat(final); err == nil {
		e.status = "done"
		e.have = fi.Size()
		return e
	}
	if fi, err := os.Stat(e.partPath); err == nil {
		e.status = "partial"
		e.have = fi.Size()
		return e
	}
	e.status = "missing"
	return e
}

func printText(w io.Writer, entries []blobEntry, ref registry.Reference, platform, cacheDir, authHeader string) error {
	var total, remaining int64
	counts := map[string]int{}
	for _, e := range entries {
		total += e.size
		counts[e.status]++
		switch e.status {
		case "partial":
			if e.size > e.have {
				remaining += e.size - e.have
			}
		case "missing":
			remaining += e.size
		}
	}

	fmt.Fprintf(w, "image:      %s\n", ref.RefString())
	fmt.Fprintf(w, "platform:   %s\n", platform)
	fmt.Fprintf(w, "cache dir:  %s\n", cacheDir)
	fmt.Fprintf(w, "summary:    %d blobs, %s total — %d done, %d partial, %d missing (%s to download)\n",
		len(entries), humanBytes(total),
		counts["done"], counts["partial"], counts["missing"],
		humanBytes(remaining))
	fmt.Fprintln(w)

	if authHeader != "" {
		fmt.Fprintln(w, "Authorization header (bearer tokens may expire in a few minutes — rerun this")
		fmt.Fprintln(w, "command to refresh if your downloader takes long to start):")
		fmt.Fprintf(w, "  Authorization: %s\n\n", authHeader)
	} else {
		fmt.Fprintln(w, "Authorization: (not required)")
		fmt.Fprintln(w)
	}

	width := len(fmt.Sprintf("%d", len(entries)))
	for i, e := range entries {
		label := e.status
		if e.status == "partial" && e.size > 0 {
			pct := float64(e.have) / float64(e.size) * 100
			label = fmt.Sprintf("partial (%s / %.1f%%)", humanBytes(e.have), pct)
		}
		fmt.Fprintf(w, "[%*d/%d] %-6s %s  %10s  %s\n",
			width, i+1, len(entries), e.kind, shortHex(e.digest), humanBytes(e.size), label)
		if e.status == "done" {
			continue
		}
		fmt.Fprintf(w, "        url:  %s\n", e.url)
		fmt.Fprintf(w, "        save: %s\n", e.savePath)
		if e.status == "partial" {
			fmt.Fprintf(w, "        note: existing partial at %s (%s on disk).\n", e.partPath, humanBytes(e.have))
			fmt.Fprintln(w, "              Saving fresh to the 'save' path above is safe — barge picks")
			fmt.Fprintln(w, "              the final file and ignores the stale .part. To resume in place")
			fmt.Fprintln(w, "              with a tool that supports byte-range continues, rename the")
			fmt.Fprintln(w, "              .part file to the save path first.")
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Next steps:")
	fmt.Fprintln(w, "  1. Download each 'missing' or 'partial' URL above to the listed 'save' path,")
	fmt.Fprintln(w, "     sending the Authorization header shown at the top with the request.")
	fmt.Fprintf(w, "  2. Re-run: barge pull %s\n", ref.RefString())
	fmt.Fprintln(w, "     barge will verify sha256 of each cached blob and pack the image.")
	return nil
}

func printAria2(w io.Writer, entries []blobEntry, authHeader string) error {
	fmt.Fprintln(w, "# aria2 input file generated by barge urls")
	fmt.Fprintln(w, "# Usage: aria2c -i <this-file> -c -x 16 -s 16 -k 10M")
	fmt.Fprintln(w, "# Bearer tokens expire quickly; regenerate this file if aria2 takes long to start.")
	fmt.Fprintln(w)
	for _, e := range entries {
		if e.status == "done" {
			continue
		}
		fmt.Fprintln(w, e.url)
		if authHeader != "" {
			fmt.Fprintf(w, "  header=Authorization: %s\n", authHeader)
		}
		fmt.Fprintf(w, "  out=%s\n", filepath.Base(e.savePath))
		fmt.Fprintf(w, "  dir=%s\n", filepath.Dir(e.savePath))
	}
	return nil
}

// shortHex returns the first 12 hex characters of a sha256 digest for display.
func shortHex(d string) string {
	h := strings.TrimPrefix(d, "sha256:")
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
