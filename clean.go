package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func runClean(args []string) error {
	var all bool

	fs := flag.NewFlagSet("clean", flag.ExitOnError)
	fs.BoolVar(&all, "all", false, "also remove cached blobs")
	fs.BoolVar(&all, "a", false, "alias of --all")
	fs.Usage = func() {
		name := filepath.Base(os.Args[0])
		fmt.Fprintf(os.Stderr, "Usage: %s clean [--all|-a]\n\n", name)
		fmt.Fprintln(os.Stderr, "Removes .part files by default; pass --all to also purge cached blobs.")
		fmt.Fprintln(os.Stderr, "\nFlags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return fmt.Errorf("clean takes no positional arguments")
	}

	dir, err := blobsDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "cache directory does not exist; nothing to clean.")
			return nil
		}
		return err
	}

	var (
		partCount, blobCount int
		partBytes, blobBytes int64
	)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(dir, e.Name())
		isPart := strings.HasSuffix(e.Name(), ".part")
		switch {
		case isPart:
			if err := os.Remove(path); err == nil {
				partCount++
				partBytes += info.Size()
			}
		case all:
			if err := os.Remove(path); err == nil {
				blobCount++
				blobBytes += info.Size()
			}
		}
	}

	fmt.Fprintf(os.Stderr, "Removed %d .part file(s), freed %s\n", partCount, humanBytes(partBytes))
	if all {
		fmt.Fprintf(os.Stderr, "Removed %d cached blob(s), freed %s\n", blobCount, humanBytes(blobBytes))
		fmt.Fprintf(os.Stderr, "Total freed: %s\n", humanBytes(partBytes+blobBytes))
	} else if remaining := len(entries) - partCount; remaining > 0 {
		fmt.Fprintf(os.Stderr, "Kept %d cached blob(s) (use --all to remove)\n", remaining)
	}
	return nil
}
