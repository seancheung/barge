package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const userAgent = "barge/0.1"

// ManifestAccepts lists every manifest media type this client understands.
var ManifestAccepts = []string{
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.oci.image.index.v1+json",
}

// Client is a minimal Registry V2 client. No third-party dependencies.
type Client struct {
	HTTP *http.Client
	Auth *AuthConfig

	// MaxRetries is the number of retries on transient failures (network error,
	// 5xx, EOF). <= 0 means no retries. Default applied via maxRetries() is 3.
	MaxRetries int
	// OnRetry is invoked before sleeping before each retry attempt.
	OnRetry func(op string, attempt, max int, delay time.Duration, err error)

	tokens sync.Map // key: "<registry>|<repo>" -> token
}

// NewClient builds a Client. If proxyURL is empty, HTTPS_PROXY/HTTP_PROXY env
// vars are honored. auth may be nil for anonymous pulls.
func NewClient(proxyURL string, auth *AuthConfig) (*Client, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("unexpected default transport type")
	}
	tr := base.Clone()
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy url %q: %w", proxyURL, err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &Client{HTTP: &http.Client{Transport: tr}, Auth: auth}, nil
}

func (c *Client) maxRetries() int {
	if c.MaxRetries < 0 {
		return 0
	}
	return c.MaxRetries
}

// retry runs fn with exponential backoff on retryable errors.
//
// The consecutive-failure counter resets to 0 whenever progressing() returns
// true (i.e. the attempt made forward progress, such as writing new bytes to a
// .part file). That way --retries N is a cap on *consecutive* failures without
// progress, not a cap on total attempts over the blob's lifetime. Pass
// progressing=nil to disable the reset behavior (used for requests where
// "progress" is meaningless, e.g. manifest fetches).
func (c *Client) retry(ctx context.Context, op string, progressing func() bool, fn func() error) error {
	var lastErr error
	max := c.maxRetries()
	attempt := 0
	for {
		if attempt > 0 {
			delay := backoff(attempt)
			if c.OnRetry != nil {
				c.OnRetry(op, attempt, max, delay, lastErr)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		err := fn()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		if !isRetryable(err) {
			return err
		}
		lastErr = err
		if progressing != nil && progressing() {
			attempt = 0 // made progress; reset the budget
		}
		attempt++
		if attempt > max {
			return fmt.Errorf("giving up after %d consecutive failures without progress: %w", max, lastErr)
		}
	}
}

func backoff(attempt int) time.Duration {
	d := time.Second << (attempt - 1) // 1s, 2s, 4s, 8s, ...
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// isRetryable reports whether an error is worth retrying (network flake, 5xx,
// EOF). Client-side errors (4xx, sha256 mismatch, context cancel) are not.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	s := err.Error()
	if strings.Contains(s, "sha256 mismatch") {
		return false
	}
	for _, code := range []string{" 400 ", " 401 ", " 403 ", " 404 ", " 405 ", " 409 "} {
		if strings.Contains(s, code) {
			return false
		}
	}
	for _, k := range []string{
		"EOF", "reset", "timeout", "refused", "broken pipe",
		" 500 ", " 502 ", " 503 ", " 504 ", " 429 ",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// GetManifest returns (body, contentType, digest). Retries on transient errors.
func (c *Client) GetManifest(ctx context.Context, ref Reference) (body []byte, contentType, digest string, err error) {
	err = c.retry(ctx, "manifest "+ref.Repository, nil, func() error {
		var e error
		body, contentType, digest, e = c.getManifestOnce(ctx, ref)
		return e
	})
	return
}

func (c *Client) getManifestOnce(ctx context.Context, ref Reference) ([]byte, string, string, error) {
	id := ref.Tag
	if ref.Digest != "" {
		id = ref.Digest
	}
	u := fmt.Sprintf("https://%s/v2/%s/manifests/%s", ref.Registry, ref.Repository, id)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("Accept", strings.Join(ManifestAccepts, ", "))
	resp, err := c.do(req, ref.Repository)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, "", "", fmt.Errorf("GET %s: %s: %s", u, resp.Status, strings.TrimSpace(string(b)))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "", err
	}
	return body, resp.Header.Get("Content-Type"), resp.Header.Get("Docker-Content-Digest"), nil
}

// DownloadBlob writes a blob to dstPath with resume support and sha256 check.
// Retries transient failures; the on-disk .part file preserves progress across
// attempts so we never restart from zero. The retry budget resets whenever an
// attempt grows the .part file — i.e. --retries caps *consecutive* stalled
// failures, not total attempts.
//
// The progress callback receives the *total* bytes downloaded for this blob so
// far (an absolute count, monotonically non-decreasing). Callers that want
// deltas can subtract the previous value themselves.
func (c *Client) DownloadBlob(ctx context.Context, ref Reference, digest, dstPath string, progress func(int64)) error {
	partPath := dstPath + ".part"
	var lastSize int64
	if fi, err := os.Stat(partPath); err == nil {
		lastSize = fi.Size()
	}
	progressing := func() bool {
		var cur int64
		if fi, err := os.Stat(partPath); err == nil {
			cur = fi.Size()
		}
		if cur > lastSize {
			lastSize = cur
			return true
		}
		return false
	}
	return c.retry(ctx, "blob "+shortDigest(digest), progressing, func() error {
		return c.downloadBlobOnce(ctx, ref, digest, dstPath, progress)
	})
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func (c *Client) downloadBlobOnce(ctx context.Context, ref Reference, digest, dstPath string, progress func(int64)) error {
	if fi, err := os.Stat(dstPath); err == nil {
		if err := verifySHA256(dstPath, digest); err == nil {
			if progress != nil {
				progress(fi.Size())
			}
			return nil
		}
		_ = os.Remove(dstPath)
	}
	partPath := dstPath + ".part"
	var offset int64
	if fi, err := os.Stat(partPath); err == nil {
		offset = fi.Size()
	}

	u := fmt.Sprintf("https://%s/v2/%s/blobs/%s", ref.Registry, ref.Repository, digest)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.do(req, ref.Repository)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var out *os.File
	switch resp.StatusCode {
	case http.StatusPartialContent:
		out, err = os.OpenFile(partPath, os.O_APPEND|os.O_WRONLY, 0o644)
	case http.StatusOK:
		offset = 0
		out, err = os.OpenFile(partPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	default:
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: %s: %s", u, resp.Status, strings.TrimSpace(string(b)))
	}
	if err != nil {
		return err
	}
	current := offset
	if progress != nil {
		progress(current)
	}
	reader := &progressReader{
		r: resp.Body,
		cb: func(n int64) {
			current += n
			if progress != nil {
				progress(current)
			}
		},
	}
	if _, err := io.Copy(out, reader); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := verifySHA256(partPath, digest); err != nil {
		return err
	}
	return os.Rename(partPath, dstPath)
}

type progressReader struct {
	r  io.Reader
	cb func(int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 && p.cb != nil {
		p.cb(int64(n))
	}
	return n, err
}

func verifySHA256(path, digest string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got != digest {
		return fmt.Errorf("sha256 mismatch for %s: expected %s, got %s", path, digest, got)
	}
	return nil
}

// do executes a request, auto-handling a single 401 auth challenge (Bearer or
// Basic) and retrying once with credentials. Transient errors bubble up to
// the outer retry loop.
func (c *Client) do(req *http.Request, repo string) (*http.Response, error) {
	registry := req.URL.Host
	req.Header.Set("User-Agent", userAgent)
	key := registry + "|" + repo
	if tok, ok := c.tokens.Load(key); ok {
		req.Header.Set("Authorization", "Bearer "+tok.(string))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenge := resp.Header.Get("Www-Authenticate")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	req2 := req.Clone(req.Context())
	lower := strings.ToLower(challenge)
	switch {
	case strings.HasPrefix(lower, "bearer "):
		token, err := c.fetchToken(req.Context(), challenge, registry, repo)
		if err != nil {
			return nil, err
		}
		c.tokens.Store(key, token)
		req2.Header.Set("Authorization", "Bearer "+token)
	case strings.HasPrefix(lower, "basic "):
		creds, ok := c.credsFor(registry)
		if !ok {
			return nil, fmt.Errorf("%s requires credentials; pass --username/--password or run `docker login %s`", registry, registry)
		}
		req2.SetBasicAuth(creds.Username, creds.Password)
	default:
		return nil, fmt.Errorf("unsupported auth challenge from %s: %q", registry, challenge)
	}
	return c.HTTP.Do(req2)
}

func (c *Client) credsFor(registry string) (Credentials, bool) {
	if c.Auth == nil {
		return Credentials{}, false
	}
	return c.Auth.For(registry)
}

var challengeRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

func (c *Client) fetchToken(ctx context.Context, challenge, registry, repo string) (string, error) {
	params := map[string]string{}
	for _, m := range challengeRe.FindAllStringSubmatch(challenge[7:], -1) {
		params[strings.ToLower(m[1])] = m[2]
	}
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("no realm in auth challenge")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if svc := params["service"]; svc != "" {
		q.Set("service", svc)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	if creds, ok := c.credsFor(registry); ok {
		switch {
		case creds.Username != "" || creds.Password != "":
			req.SetBasicAuth(creds.Username, creds.Password)
		case creds.IdentityToken != "":
			req.SetBasicAuth("<token>", creds.IdentityToken)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token GET %s: %s: %s", u, resp.Status, strings.TrimSpace(string(b)))
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.Token != "" {
		return body.Token, nil
	}
	return body.AccessToken, nil
}
