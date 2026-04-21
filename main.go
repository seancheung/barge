package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		printTopUsage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "pull":
		if err := runPull(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "clean":
		if err := runClean(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "status":
		if err := runStatus(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "-v", "--version", "-version":
		fmt.Println(version)
	case "-h", "--help", "-help", "help":
		printTopUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		printTopUsage()
		os.Exit(2)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}

func printTopUsage() {
	name := filepath.Base(os.Args[0])
	fmt.Fprintf(os.Stderr, "barge %s — pull container images from any registry into a docker-load compatible tar\n\n", version)
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintf(os.Stderr, "  %s pull <image> [flags]   pull an image and pack it into a tar\n", name)
	fmt.Fprintf(os.Stderr, "  %s clean [--all]          remove .part files (--all also clears blob cache)\n", name)
	fmt.Fprintf(os.Stderr, "  %s status                 show cache status\n", name)
	fmt.Fprintf(os.Stderr, "  %s --version              print version\n\n", name)
	fmt.Fprintf(os.Stderr, "Run '%s pull --help' for pull flags.\n", name)
}

// bargeHome returns the data root for barge. Defaults to ~/.barge, overridable
// via the BARGE_HOME environment variable.
func bargeHome() (string, error) {
	if env := os.Getenv("BARGE_HOME"); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine user home directory: %w", err)
	}
	return filepath.Join(home, ".barge"), nil
}

// blobsDir returns the blob cache directory ($BARGE_HOME/blobs).
func blobsDir() (string, error) {
	home, err := bargeHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "blobs"), nil
}

func digestHex(d string) string { return strings.TrimPrefix(d, "sha256:") }

func humanBytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(u), 0
	for x := n / u; x >= u; x /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.2f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}
