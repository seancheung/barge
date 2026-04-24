package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"github.com/theoxuanx/barge/internal/registry"
	"github.com/theoxuanx/barge/internal/tarball"
)

type pullOptions struct {
	output        string
	platform      string
	proxyURL      string
	concurrency   int
	retries       int
	username      string
	password      string
	passwordStdin bool
	dockerConfig  string
}

func runPull(args []string) error {
	var opts pullOptions
	defaultPlatform := "linux/" + runtime.GOARCH

	fs := flag.NewFlagSet("pull", flag.ExitOnError)
	fs.StringVar(&opts.output, "output", "", "output tar path (default <repo>_<tag>.tar)")
	fs.StringVar(&opts.output, "o", "", "alias of --output")
	fs.StringVar(&opts.platform, "platform", defaultPlatform, "target platform os/arch[/variant] (defaults to host arch)")
	fs.StringVar(&opts.platform, "p", defaultPlatform, "alias of --platform")
	fs.StringVar(&opts.proxyURL, "proxy", "", "HTTP/HTTPS proxy URL (falls back to HTTPS_PROXY env)")
	fs.StringVar(&opts.proxyURL, "x", "", "alias of --proxy")
	fs.IntVar(&opts.concurrency, "concurrency", 3, "number of layers to download in parallel")
	fs.IntVar(&opts.concurrency, "c", 3, "alias of --concurrency")
	fs.IntVar(&opts.retries, "retries", 3, "max consecutive failures without progress before giving up")
	fs.IntVar(&opts.retries, "r", 3, "alias of --retries")
	fs.StringVar(&opts.username, "username", "", "registry username")
	fs.StringVar(&opts.username, "u", "", "alias of --username")
	fs.StringVar(&opts.password, "password", "", "registry password or token (prefer --password-stdin)")
	fs.BoolVar(&opts.passwordStdin, "password-stdin", false, "read password/token from stdin")
	fs.StringVar(&opts.dockerConfig, "docker-config", "", "path to docker config.json (default $DOCKER_CONFIG or ~/.docker/config.json)")

	fs.Usage = func() {
		name := filepath.Base(os.Args[0])
		fmt.Fprintf(os.Stderr, "Usage: %s pull <image> [flags]\n\nExamples:\n", name)
		fmt.Fprintf(os.Stderr, "  %s pull nginx:1.25\n", name)
		fmt.Fprintf(os.Stderr, "  %s pull -p linux/arm64 ghcr.io/owner/repo:tag\n", name)
		fmt.Fprintf(os.Stderr, "  %s pull -x http://127.0.0.1:7890 -o out.tar alpine:3.20\n", name)
		fmt.Fprintf(os.Stderr, "  echo $GH_PAT | %s pull --password-stdin -u myuser ghcr.io/org/private:tag\n\n", name)
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
	return doPull(fs.Arg(0), opts)
}

func doPull(image string, opts pullOptions) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	ref, err := registry.ParseReference(image)
	if err != nil {
		return fmt.Errorf("parse image reference: %w", err)
	}
	fmt.Fprintf(os.Stderr, "image:    %s\nplatform: %s\n", ref.RefString(), opts.platform)

	auth, err := buildAuth(ref, opts.dockerConfig, opts.username, opts.password, opts.passwordStdin)
	if err != nil {
		return err
	}

	client, err := registry.NewClient(opts.proxyURL, auth)
	if err != nil {
		return err
	}
	client.MaxRetries = opts.retries

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

	configState := newLayerState(manifest.Config.Digest, manifest.Config.Size)
	configPath := filepath.Join(dir, digestHex(manifest.Config.Digest))
	seedFromDisk(configState, configPath)

	layerStates := make([]*layerState, len(manifest.Layers))
	layerPaths := make([]string, len(manifest.Layers))
	for i, l := range manifest.Layers {
		layerStates[i] = newLayerState(l.Digest, l.Size)
		layerPaths[i] = filepath.Join(dir, digestHex(l.Digest))
		seedFromDisk(layerStates[i], layerPaths[i])
	}

	tracker := newProgressTracker(configState, layerStates)
	go tracker.Run(ctx)

	if err := client.DownloadBlob(ctx, ref, manifest.Config.Digest, configPath, progressCallback(configState)); err != nil {
		configState.setError(err)
		tracker.Stop()
		return fmt.Errorf("download config: %w", err)
	}
	configState.setDone()

	sem := make(chan struct{}, opts.concurrency)
	errs := make(chan error, len(manifest.Layers))
	var wg sync.WaitGroup
	for i, l := range manifest.Layers {
		i, l := i, l
		state := layerStates[i]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			err := client.DownloadBlob(ctx, ref, l.Digest, layerPaths[i], progressCallback(state))
			if err != nil {
				state.setError(err)
				errs <- fmt.Errorf("layer %s: %w", l.Digest[:19], err)
				return
			}
			state.setDone()
		}()
	}
	wg.Wait()
	close(errs)
	tracker.Stop()

	var errList []error
	for e := range errs {
		errList = append(errList, e)
	}
	if len(errList) > 0 {
		return errors.Join(errList...)
	}

	output := opts.output
	if output == "" {
		output = defaultOutputName(ref)
	}

	var repoTag string
	if ref.Digest == "" {
		repoTag = ref.RefString()
	}

	layerDigests := make([]string, len(manifest.Layers))
	for i, l := range manifest.Layers {
		layerDigests[i] = l.Digest
	}

	fmt.Fprintf(os.Stderr, "packing into %s ...\n", output)
	if err := tarball.Write(output, repoTag, manifest.Config.Digest, configPath, layerDigests, layerPaths); err != nil {
		return fmt.Errorf("pack tarball: %w", err)
	}

	if info, err := os.Stat(output); err == nil {
		fmt.Fprintf(os.Stderr, "done. image id=%s size=%s\n",
			manifest.Config.Digest[:19], humanBytes(info.Size()))
	}
	return nil
}

func buildAuth(ref registry.Reference, dockerConfig, username, password string, passwordStdin bool) (*registry.AuthConfig, error) {
	auth, err := registry.LoadDockerConfig(dockerConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: load docker config: %v\n", err)
		auth = registry.NewAuthConfig()
	}

	pw := password
	if passwordStdin {
		if pw != "" {
			return nil, fmt.Errorf("--password and --password-stdin are mutually exclusive")
		}
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		pw = strings.TrimRight(string(b), "\r\n")
	}
	if username != "" || pw != "" {
		auth.Set(ref.Registry, registry.Credentials{Username: username, Password: pw})
	}
	return auth, nil
}

func defaultOutputName(ref registry.Reference) string {
	base := strings.ReplaceAll(strings.TrimPrefix(ref.Repository, "library/"), "/", "_")
	suffix := ref.Tag
	if suffix == "" {
		suffix = digestHex(ref.Digest)[:12]
	}
	return base + "_" + suffix + ".tar"
}
