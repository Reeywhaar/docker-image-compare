// Package registry is a small standard-library client for OCI/Docker v2 registries:
// reference parsing, the bearer-token handshake, manifest/index fetching, and platform
// resolution. It fetches only manifest/config metadata — never image content.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const dockerHub = "registry-1.docker.io"

// ctxKey is the context key type for request-scoped values.
type ctxKey int

const xffKey ctxKey = iota

// WithForwardedFor stashes the inbound client's X-Forwarded-For value on the context so
// outgoing registry requests can propagate it.
func WithForwardedFor(ctx context.Context, xff string) context.Context {
	if xff == "" {
		return ctx
	}
	return context.WithValue(ctx, xffKey, xff)
}

// buildUserAgent constructs the User-Agent string, embedding HOST when configured so
// registries can attribute traffic to this deployment.
func buildUserAgent(host string) string {
	if host == "" {
		return "docker-image-compare/1.0"
	}
	return "docker-image-compare/1.0 (+" + host + ")"
}

// acceptManifests lists the manifest media types we understand, in preference order.
const acceptManifests = "application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json"

// Ref is a parsed image reference: registry host, repository, and a tag or digest.
type Ref struct {
	registry string
	repo     string
	ref      string // tag or "sha256:..." digest
}

func (r Ref) String() string { return r.registry + "/" + r.repo + ":" + r.ref }

// ParseRef splits a user-supplied image name into registry/repo/ref, applying Docker Hub
// defaults (library/ prefix for single-segment names, "latest" tag).
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, fmt.Errorf("empty image reference")
	}

	name := s
	ref := ""

	// A digest (@sha256:...) takes precedence over a tag.
	if i := strings.Index(name, "@"); i >= 0 {
		ref = name[i+1:]
		name = name[:i]
	} else if i := strings.LastIndex(name, ":"); i >= 0 {
		// Only treat the trailing colon as a tag separator if it's after the last "/"
		// (otherwise it's a registry host:port).
		if strings.LastIndex(name, "/") < i {
			ref = name[i+1:]
			name = name[:i]
		}
	}
	if ref == "" {
		ref = "latest"
	}
	if name == "" {
		return Ref{}, fmt.Errorf("missing repository name in %q", s)
	}

	registry := dockerHub
	repo := name
	if i := strings.Index(name, "/"); i >= 0 {
		first := name[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			registry = first
			repo = name[i+1:]
		}
	}
	// Docker Hub official images live under library/.
	if registry == dockerHub && !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	if repo == "" {
		return Ref{}, fmt.Errorf("missing repository name in %q", s)
	}
	return Ref{registry: registry, repo: repo, ref: ref}, nil
}

// NormalizeName lowercases the registry+repository part of a reference, leaving the tag or
// digest untouched. Repository names must be lowercase; tags are case-sensitive.
func NormalizeName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "@"); i >= 0 { // name@digest
		return strings.ToLower(s[:i]) + s[i:]
	}
	if i := strings.LastIndex(s, ":"); i >= 0 && strings.LastIndex(s, "/") < i { // name:tag
		return strings.ToLower(s[:i]) + s[i:]
	}
	return strings.ToLower(s) // no tag/digest — all name
}

// Platform identifies an OS/architecture target within a multi-arch image.
type Platform struct {
	os      string
	arch    string
	variant string
}

func (p Platform) String() string {
	if p.variant != "" {
		return p.os + "/" + p.arch + "/" + p.variant
	}
	return p.os + "/" + p.arch
}

// ParsePlatform parses an "os/arch[/variant]" string into a Platform.
func ParsePlatform(s string) Platform {
	parts := strings.Split(strings.TrimSpace(s), "/")
	var p Platform
	if len(parts) > 0 {
		p.os = parts[0]
	}
	if len(parts) > 1 {
		p.arch = parts[1]
	}
	if len(parts) > 2 {
		p.variant = parts[2]
	}
	return p
}

// ParsePlatforms converts a list of "os/arch[/variant]" strings into Platform values.
func ParsePlatforms(ss []string) []Platform {
	out := make([]Platform, 0, len(ss))
	for _, s := range ss {
		out = append(out, ParsePlatform(s))
	}
	return out
}

// CommonPlatforms returns the platforms present in every list, in the order of the first.
func CommonPlatforms(sets [][]Platform) []Platform {
	if len(sets) == 0 {
		return nil
	}
	common := sets[0]
	for _, s := range sets[1:] {
		common = intersectPlatforms(common, s)
	}
	return common
}

// intersectPlatforms returns platforms present in both lists, in the order of the first.
func intersectPlatforms(a, b []Platform) []Platform {
	set := make(map[Platform]bool, len(b))
	for _, p := range b {
		set[p] = true
	}
	var out []Platform
	for _, p := range a {
		if set[p] {
			out = append(out, p)
		}
	}
	return out
}

// PickPlatform chooses the selected platform if it's available, else linux/amd64, else first.
func PickPlatform(common []Platform, selected string) Platform {
	if selected != "" {
		sp := ParsePlatform(selected)
		for _, p := range common {
			if p == sp {
				return p
			}
		}
	}
	for _, p := range common {
		if p.os == "linux" && p.arch == "amd64" && p.variant == "" {
			return p
		}
	}
	return common[0]
}

// Layer is a single image layer: its content digest and (compressed) size in bytes.
type Layer struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// basicCreds holds optional registry credentials (unused for now — anonymous access).
type basicCreds struct {
	username string
	password string
}

// cacheTTL bounds how long fetched registry data is reused. Kept short so a moved tag
// (repo:latest) doesn't serve stale layers for long, while still collapsing the burst of
// repeated lookups a single add/remove/compare interaction triggers.
const cacheTTL = time.Minute

type cachedManifest struct {
	body        []byte
	contentType string
	expires     time.Time
}

type cachedToken struct {
	token   string
	expires time.Time
}

type cachedConfig struct {
	cfg     imageConfig
	expires time.Time
}

// Client talks to OCI/Docker v2 registries using only the standard library.
type Client struct {
	hc        *http.Client
	userAgent string
	creds     map[string]basicCreds // host -> creds; nil for now (anonymous only)

	mu        sync.Mutex
	manifests map[string]cachedManifest // key: registry/repo@ref
	tokens    map[string]cachedToken    // key: registry/repo
	configs   map[string]cachedConfig   // key: registry/repo@digest
}

// NewClient returns a registry client, embedding the HOST env var in its User-Agent.
func NewClient() *Client {
	return &Client{
		hc:        &http.Client{Timeout: 30 * time.Second},
		userAgent: buildUserAgent(os.Getenv("HOST")),
		manifests: map[string]cachedManifest{},
		tokens:    map[string]cachedToken{},
		configs:   map[string]cachedConfig{},
	}
}

// applyHeaders sets the User-Agent on every request and forwards X-Forwarded-For if present.
func (rc *Client) applyHeaders(req *http.Request) {
	req.Header.Set("User-Agent", rc.userAgent)
	if xff, ok := req.Context().Value(xffKey).(string); ok && xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
}

type challenge struct {
	realm   string
	service string
}

// parseChallenge extracts realm/service from a Bearer WWW-Authenticate header.
func parseChallenge(h string) challenge {
	var c challenge
	h = strings.TrimSpace(h)
	if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return c
	}
	for _, part := range splitParams(h[len("bearer "):]) {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.Trim(strings.TrimSpace(kv[1]), `"`)
		switch key {
		case "realm":
			c.realm = val
		case "service":
			c.service = val
		}
	}
	return c
}

// splitParams splits a comma-separated parameter list, ignoring commas inside quotes.
func splitParams(s string) []string {
	var out []string
	var b strings.Builder
	inQuote := false
	for _, r := range s {
		switch r {
		case '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case ',':
			if inQuote {
				b.WriteRune(r)
			} else {
				out = append(out, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// token obtains a bearer token for pulling repo from host, caching the result.
func (rc *Client) token(ctx context.Context, host, repo string) (string, error) {
	key := host + "/" + repo
	rc.mu.Lock()
	if t, ok := rc.tokens[key]; ok && time.Now().Before(t.expires) {
		rc.mu.Unlock()
		return t.token, nil
	}
	rc.mu.Unlock()

	// Probe /v2/ to read the auth challenge.
	probe := fmt.Sprintf("https://%s/v2/", host)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probe, nil)
	if err != nil {
		return "", err
	}
	rc.applyHeaders(req)
	resp, err := rc.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("contacting %s: %w", host, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return "", nil // registry needs no token
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return "", fmt.Errorf("registry %s returned %s", host, resp.Status)
	}

	ch := parseChallenge(resp.Header.Get("WWW-Authenticate"))
	if ch.realm == "" {
		return "", nil
	}

	u, err := url.Parse(ch.realm)
	if err != nil {
		return "", fmt.Errorf("bad auth realm %q: %w", ch.realm, err)
	}
	q := u.Query()
	if ch.service != "" {
		q.Set("service", ch.service)
	}
	q.Set("scope", "repository:"+repo+":pull")
	u.RawQuery = q.Encode()

	treq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	rc.applyHeaders(treq)
	if c, ok := rc.creds[host]; ok {
		treq.SetBasicAuth(c.username, c.password)
	}
	tresp, err := rc.hc.Do(treq)
	if err != nil {
		return "", fmt.Errorf("fetching auth token: %w", err)
	}
	defer tresp.Body.Close()
	if tresp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth token request failed: %s", tresp.Status)
	}
	var tr struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(tresp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("decoding auth token: %w", err)
	}
	tok := tr.Token
	if tok == "" {
		tok = tr.AccessToken
	}

	rc.mu.Lock()
	rc.tokens[key] = cachedToken{token: tok, expires: time.Now().Add(cacheTTL)}
	rc.mu.Unlock()
	return tok, nil
}

// getManifest fetches the manifest (or index, or blob) at the given path reference,
// caching the raw body by registry/repo@ref.
func (rc *Client) getManifest(ctx context.Context, registry, repo, ref string) (cachedManifest, error) {
	key := registry + "/" + repo + "@" + ref
	rc.mu.Lock()
	if cm, ok := rc.manifests[key]; ok && time.Now().Before(cm.expires) {
		rc.mu.Unlock()
		return cm, nil
	}
	rc.mu.Unlock()

	tok, err := rc.token(ctx, registry, repo)
	if err != nil {
		return cachedManifest{}, err
	}

	u := fmt.Sprintf("https://%s/v2/%s/manifests/%s", registry, repo, url.PathEscape(ref))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return cachedManifest{}, err
	}
	rc.applyHeaders(req)
	req.Header.Set("Accept", acceptManifests)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := rc.hc.Do(req)
	if err != nil {
		return cachedManifest{}, fmt.Errorf("fetching manifest: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return cachedManifest{}, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		// ok
	case http.StatusNotFound:
		return cachedManifest{}, fmt.Errorf("not found: %s/%s:%s", registry, repo, ref)
	case http.StatusUnauthorized, http.StatusForbidden:
		return cachedManifest{}, fmt.Errorf("not authorized for %s/%s (private image?)", registry, repo)
	default:
		return cachedManifest{}, fmt.Errorf("registry returned %s for %s/%s:%s", resp.Status, registry, repo, ref)
	}

	cm := cachedManifest{body: body, contentType: resp.Header.Get("Content-Type"), expires: time.Now().Add(cacheTTL)}
	rc.mu.Lock()
	rc.manifests[key] = cm
	rc.mu.Unlock()
	return cm, nil
}

// rawManifest captures the union of fields across index and single-image manifests.
type rawManifest struct {
	MediaType string `json:"mediaType"`
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform struct {
			OS      string `json:"os"`
			Arch    string `json:"architecture"`
			Variant string `json:"variant"`
		} `json:"platform"`
	} `json:"manifests"`
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Layers []struct {
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	} `json:"layers"`
}

func isIndex(cm cachedManifest, m rawManifest) bool {
	if strings.Contains(cm.contentType, "manifest.list") || strings.Contains(cm.contentType, "image.index") {
		return true
	}
	return len(m.Manifests) > 0 && len(m.Layers) == 0
}

// Platforms returns the platforms available for an image reference.
func (rc *Client) Platforms(ctx context.Context, r Ref) ([]Platform, error) {
	cm, err := rc.getManifest(ctx, r.registry, r.repo, r.ref)
	if err != nil {
		return nil, err
	}
	var m rawManifest
	if err := json.Unmarshal(cm.body, &m); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}

	if isIndex(cm, m) {
		var out []Platform
		seen := map[string]bool{}
		for _, e := range m.Manifests {
			// Skip attestation / non-runnable entries (e.g. os "unknown").
			if e.Platform.OS == "" || e.Platform.OS == "unknown" || e.Platform.Arch == "unknown" {
				continue
			}
			p := Platform{os: e.Platform.OS, arch: e.Platform.Arch, variant: e.Platform.Variant}
			if !seen[p.String()] {
				seen[p.String()] = true
				out = append(out, p)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("no runnable platforms found in %s", r)
		}
		return out, nil
	}

	// Single-image manifest: read os/arch from the config blob.
	if len(m.Layers) == 0 {
		return nil, fmt.Errorf("unsupported manifest type for %s (schema v1?)", r)
	}
	cfg, err := rc.fetchConfig(ctx, r, m.Config.Digest)
	if err != nil {
		return nil, err
	}
	return []Platform{{os: cfg.OS, arch: cfg.Arch, variant: cfg.Variant}}, nil
}

// imageConfig holds the bits of the image config blob we care about.
type imageConfig struct {
	OS      string
	Arch    string
	Variant string
	Created time.Time // zero if unknown/unparsable
}

// fetchConfig fetches and parses an image config blob (os/arch + creation time).
func (rc *Client) fetchConfig(ctx context.Context, r Ref, digest string) (imageConfig, error) {
	if digest == "" {
		return imageConfig{OS: "linux", Arch: "amd64"}, nil
	}
	key := r.registry + "/" + r.repo + "@" + digest
	rc.mu.Lock()
	if c, ok := rc.configs[key]; ok && time.Now().Before(c.expires) {
		rc.mu.Unlock()
		return c.cfg, nil
	}
	rc.mu.Unlock()

	tok, err := rc.token(ctx, r.registry, r.repo)
	if err != nil {
		return imageConfig{}, err
	}
	u := fmt.Sprintf("https://%s/v2/%s/blobs/%s", r.registry, r.repo, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return imageConfig{}, err
	}
	rc.applyHeaders(req)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := rc.hc.Do(req)
	if err != nil {
		return imageConfig{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return imageConfig{}, fmt.Errorf("fetching image config: %s", resp.Status)
	}
	var cfg struct {
		OS      string `json:"os"`
		Arch    string `json:"architecture"`
		Variant string `json:"variant"`
		Created string `json:"created"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&cfg); err != nil {
		return imageConfig{}, err
	}
	ic := imageConfig{OS: cfg.OS, Arch: cfg.Arch, Variant: cfg.Variant, Created: ParseTime(cfg.Created)}
	rc.mu.Lock()
	rc.configs[key] = cachedConfig{cfg: ic, expires: time.Now().Add(cacheTTL)}
	rc.mu.Unlock()
	return ic, nil
}

// ParseTime parses an RFC3339(/Nano) timestamp, returning the zero time on failure.
func ParseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// PlatformInfo returns the layers and creation time of an image for a specific platform,
// resolving manifest lists.
func (rc *Client) PlatformInfo(ctx context.Context, r Ref, p Platform) ([]Layer, time.Time, error) {
	cm, err := rc.getManifest(ctx, r.registry, r.repo, r.ref)
	if err != nil {
		return nil, time.Time{}, err
	}
	var m rawManifest
	if err := json.Unmarshal(cm.body, &m); err != nil {
		return nil, time.Time{}, fmt.Errorf("parsing manifest: %w", err)
	}

	if isIndex(cm, m) {
		digest := ""
		for _, e := range m.Manifests {
			ep := Platform{os: e.Platform.OS, arch: e.Platform.Arch, variant: e.Platform.Variant}
			if ep == p {
				digest = e.Digest
				break
			}
		}
		if digest == "" {
			return nil, time.Time{}, fmt.Errorf("platform %s not available for %s", p, r)
		}
		sub, err := rc.getManifest(ctx, r.registry, r.repo, digest)
		if err != nil {
			return nil, time.Time{}, err
		}
		if err := json.Unmarshal(sub.body, &m); err != nil {
			return nil, time.Time{}, fmt.Errorf("parsing platform manifest: %w", err)
		}
	}

	if len(m.Layers) == 0 {
		return nil, time.Time{}, fmt.Errorf("no layers found for %s (%s)", r, p)
	}
	out := make([]Layer, 0, len(m.Layers))
	for _, l := range m.Layers {
		out = append(out, Layer{Digest: l.Digest, Size: l.Size})
	}

	// Creation time comes from the config blob (best-effort; failure is non-fatal).
	created := time.Time{}
	if cfg, err := rc.fetchConfig(ctx, r, m.Config.Digest); err == nil {
		created = cfg.Created
	}
	return out, created, nil
}
