package registry

import (
	"fmt"
	"strings"
)

const (
	DefaultRegistry = "registry-1.docker.io"
	DefaultTag      = "latest"
)

// Reference denotes an image reference, e.g. nginx:latest or ghcr.io/user/repo@sha256:xxx.
type Reference struct {
	Registry   string // e.g. registry-1.docker.io
	Repository string // e.g. library/nginx
	Tag        string // mutually exclusive with Digest
	Digest     string // e.g. sha256:xxx
}

// RefString returns a protocol-neutral reference string for manifest.json RepoTags.
func (r Reference) RefString() string {
	name := r.Repository
	if r.Registry != DefaultRegistry {
		name = r.Registry + "/" + r.Repository
	} else {
		name = strings.TrimPrefix(name, "library/")
	}
	if r.Digest != "" {
		return name + "@" + r.Digest
	}
	return name + ":" + r.Tag
}

// ParseReference parses a reference string like "nginx:1.25" or
// "ghcr.io/org/repo@sha256:...".
func ParseReference(s string) (Reference, error) {
	if s == "" {
		return Reference{}, fmt.Errorf("empty reference")
	}

	var ref Reference

	// Split off @digest.
	if i := strings.Index(s, "@"); i >= 0 {
		ref.Digest = s[i+1:]
		s = s[:i]
		if !strings.HasPrefix(ref.Digest, "sha256:") {
			return Reference{}, fmt.Errorf("only sha256 digest supported, got %q", ref.Digest)
		}
	}

	// If the first path segment contains '.' or ':' or equals "localhost",
	// treat it as the registry host.
	slash := strings.Index(s, "/")
	if slash >= 0 {
		head := s[:slash]
		if strings.ContainsAny(head, ".:") || head == "localhost" {
			ref.Registry = head
			s = s[slash+1:]
		}
	}
	if ref.Registry == "" {
		ref.Registry = DefaultRegistry
	}

	// The remainder is repo[:tag].
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s[i:], "/") {
		ref.Tag = s[i+1:]
		s = s[:i]
	}
	ref.Repository = s

	// On Docker Hub, single-segment repo names get the "library/" prefix.
	if ref.Registry == DefaultRegistry && !strings.Contains(ref.Repository, "/") {
		ref.Repository = "library/" + ref.Repository
	}

	if ref.Repository == "" {
		return Reference{}, fmt.Errorf("missing repository in %q", s)
	}
	if ref.Tag == "" && ref.Digest == "" {
		ref.Tag = DefaultTag
	}
	return ref, nil
}
