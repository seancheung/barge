package tarball

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Write assembles a docker-load compatible tar.
//   - configDigest: "sha256:..." of the image config blob
//   - configPath: local path to the config JSON file
//   - layerDigests / layerPaths: in manifest order (same length); each file is
//     a gzip blob as returned by the registry
//   - repoTag: reference string for manifest.json RepoTags (e.g. nginx:latest);
//     may be empty for digest-only pulls
func Write(
	outPath string,
	repoTag string,
	configDigest string,
	configPath string,
	layerDigests []string,
	layerPaths []string,
) error {
	if len(layerDigests) != len(layerPaths) {
		return fmt.Errorf("layer digest/path count mismatch: %d vs %d", len(layerDigests), len(layerPaths))
	}

	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	tw := tar.NewWriter(f)

	configName := digestHex(configDigest) + ".json"
	if err := addFile(tw, configName, configPath); err != nil {
		return err
	}

	layerNames := make([]string, len(layerPaths))
	for i, p := range layerPaths {
		name := digestHex(layerDigests[i]) + ".tar.gz"
		if err := addFile(tw, name, p); err != nil {
			return err
		}
		layerNames[i] = name
	}

	var repoTags []string
	if repoTag != "" {
		repoTags = []string{repoTag}
	}
	manifest := []map[string]any{{
		"Config":   configName,
		"RepoTags": repoTags,
		"Layers":   layerNames,
	}}
	mb, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := addBytes(tw, "manifest.json", mb); err != nil {
		return err
	}
	return tw.Close()
}

func digestHex(d string) string {
	return strings.TrimPrefix(d, "sha256:")
}

func addFile(tw *tar.Writer, name, src string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     fi.Size(),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}

func addBytes(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}
