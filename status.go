package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	fs.Usage = func() {
		name := filepath.Base(os.Args[0])
		fmt.Fprintf(os.Stderr, "Usage: %s status\n\nShow cache directory path, number and size of completed blobs, and .part files.\n", name)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return fmt.Errorf("status takes no positional arguments")
	}

	home, err := bargeHome()
	if err != nil {
		return err
	}
	dir, err := blobsDir()
	if err != nil {
		return err
	}

	fmt.Printf("Data dir:   %s\n", home)
	fmt.Printf("Blobs dir:  %s\n", dir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("Directory does not exist; cache is empty.")
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
		if strings.HasSuffix(e.Name(), ".part") {
			partCount++
			partBytes += info.Size()
		} else {
			blobCount++
			blobBytes += info.Size()
		}
	}

	fmt.Printf("Blobs:      %d, %s\n", blobCount, humanBytes(blobBytes))
	fmt.Printf(".part:      %d, %s\n", partCount, humanBytes(partBytes))
	fmt.Printf("Total:      %s\n", humanBytes(blobBytes+partBytes))
	return nil
}
