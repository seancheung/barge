package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Descriptor is the structure shared by config, layer, and manifest-list entries.
type Descriptor struct {
	MediaType string    `json:"mediaType"`
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	Platform  *Platform `json:"platform,omitempty"`
}

type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

// Manifest is a single-image manifest (Docker V2 or OCI).
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// Index is a manifest list / OCI index.
type Index struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Manifests     []Descriptor `json:"manifests"`
}

// IsIndex reports whether the given media type denotes a manifest list / index.
func IsIndex(mt string) bool {
	return mt == "application/vnd.docker.distribution.manifest.list.v2+json" ||
		mt == "application/vnd.oci.image.index.v1+json"
}

// ResolveManifest fetches the manifest for a reference. If it is a manifest
// list / index, one child is picked by platform and re-fetched.
// platform is "os/arch" or "os/arch/variant" (e.g. "linux/amd64", "linux/arm64/v8").
func (c *Client) ResolveManifest(ctx context.Context, ref Reference, platform string) (*Manifest, []byte, string, error) {
	body, mt, digest, err := c.GetManifest(ctx, ref)
	if err != nil {
		return nil, nil, "", err
	}
	if IsIndex(mt) {
		var idx Index
		if err := json.Unmarshal(body, &idx); err != nil {
			return nil, nil, "", fmt.Errorf("parse manifest index: %w", err)
		}
		picked, err := pickPlatform(idx.Manifests, platform)
		if err != nil {
			return nil, nil, "", err
		}
		sub := ref
		sub.Tag = ""
		sub.Digest = picked.Digest
		body, mt, digest, err = c.GetManifest(ctx, sub)
		if err != nil {
			return nil, nil, "", err
		}
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil, "", fmt.Errorf("parse manifest: %w", err)
	}
	if len(m.Layers) == 0 {
		return nil, nil, "", fmt.Errorf("manifest has no layers (mediaType=%s)", mt)
	}
	return &m, body, digest, nil
}

func pickPlatform(list []Descriptor, want string) (*Descriptor, error) {
	parts := strings.Split(want, "/")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid platform %q (want os/arch[/variant])", want)
	}
	wOS, wArch := parts[0], parts[1]
	wVar := ""
	if len(parts) >= 3 {
		wVar = parts[2]
	}
	var fallback *Descriptor
	for i := range list {
		d := list[i]
		if d.Platform == nil {
			continue
		}
		if d.Platform.OS == wOS && d.Platform.Architecture == wArch {
			if wVar == "" || d.Platform.Variant == wVar {
				return &d, nil
			}
			if fallback == nil {
				fallback = &d
			}
		}
	}
	if fallback != nil {
		return fallback, nil
	}
	var avail []string
	for _, d := range list {
		if d.Platform != nil {
			p := d.Platform.OS + "/" + d.Platform.Architecture
			if d.Platform.Variant != "" {
				p += "/" + d.Platform.Variant
			}
			avail = append(avail, p)
		}
	}
	return nil, fmt.Errorf("no matching platform %q; available: %s", want, strings.Join(avail, ", "))
}
