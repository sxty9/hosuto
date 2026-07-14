// Package modrinth is hosuto's read-only client for the Modrinth v2 API: it searches mods, resolves
// a project to a concrete downloadable version, and fetches the jars.
//
// It exists to hold ONE invariant that the rest of hosuto depends on and cannot recover later:
// a mod's ENVIRONMENT. Modrinth publishes client_side/server_side (required|optional|unsupported)
// per project, and that pair — nothing else — decides where a jar is allowed to go:
//
//   - server_side == "unsupported" → the mod must never be installed on the server;
//   - client_side == "unsupported" → the mod must never be shipped in the client export.
//
// Both fields are carried verbatim into store.Mod. When Modrinth omits one, or returns a value
// outside the vocabulary, it is recorded as "unknown" (see env). It is never inferred from the
// category, the filename or anything else: a wrong guess here silently corrupts a player's client
// or crashes the server, and neither failure points back at the guess.
//
// The client fails closed: no checksum means no download (Download), no matching version means an
// error rather than a "close enough" jar (Resolve), and a version that does not actually list the
// requested game version and loader is dropped even if the API returned it (Versions).
//
// Modrinth's terms require a descriptive User-Agent and cap callers at 300 requests per minute.
// The agent comes from config (hconfig id "modrinthUserAgent"); the rate cap is honoured locally by
// a token bucket, so a member hammering the search box degrades into waiting rather than into 429s.
package modrinth

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"hosuto/internal/store"
)

// DefaultBaseURL is Modrinth's public API root.
const DefaultBaseURL = "https://api.modrinth.com/v2"

// Environment vocabulary. Unknown is hosuto's own value, not Modrinth's: it marks "the API did not
// tell us", which is a fact the operator may act on, unlike a guess.
const (
	EnvRequired    = "required"
	EnvOptional    = "optional"
	EnvUnsupported = "unsupported"
	EnvUnknown     = "unknown"
)

const (
	// apiTimeout bounds a metadata call. The UI blocks on these, so they must fail fast.
	apiTimeout = 10 * time.Second
	// downloadTimeout bounds a jar fetch. It cannot share apiTimeout: http.Client.Timeout covers the
	// body read, and a 60 MB modpack jar over a slow link legitimately outlives ten seconds.
	downloadTimeout = 10 * time.Minute

	// cacheTTL keeps the search box and the mod tab off the network on every keystroke and re-render.
	// Short enough that a freshly published version shows up while the member is still looking.
	cacheTTL = 60 * time.Second
	cacheMax = 256

	// Modrinth's published cap is 300 requests per minute.
	rateBurst    = 300.0
	ratePerSec   = rateBurst / 60.0
	hashBatch    = 100     // POST /version_files hashes per request
	maxBodyBytes = 8 << 20 // an API response this big is a bug or an attack, not a mod list
	maxJarBytes  = 1 << 30 // 1 GiB: a jar larger than this is not a mod
)

var (
	// ErrNotFound is a 404 from Modrinth: no such project, version or hash.
	ErrNotFound = errors.New("modrinth: not found")
	// ErrNoVersion means the project exists but publishes nothing for this game version + loader.
	// This is the common, expected case (a mod that has not updated yet) and must surface as such.
	ErrNoVersion = errors.New("modrinth: no version for this game version and loader")
	// ErrRateLimited is a 429. Returned rather than retried: the caller is a request handler.
	ErrRateLimited = errors.New("modrinth: rate limited")
	// ErrChecksum means the bytes on disk are not the bytes Modrinth promised. Always fatal.
	ErrChecksum = errors.New("modrinth: checksum mismatch")
)

// Hit is a project as the UI shows it — the search result and the project page are the same shape,
// because the only fields hosuto needs from either are the identity, the blurb and the environment.
type Hit struct {
	ProjectID   string `json:"projectId"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Description string `json:"description"`
	IconURL     string `json:"iconUrl,omitempty"`
	ClientSide  string `json:"clientSide"` // required | optional | unsupported | unknown
	ServerSide  string `json:"serverSide"`
	Downloads   int    `json:"downloads"`
}

// ServerSafe reports whether this mod may be installed on the server.
// Note "unknown" is permitted: it means Modrinth was silent, and the operator, not this package,
// decides what to do about that. Only an explicit "unsupported" is a hard no.
func (h Hit) ServerSafe() bool { return h.ServerSide != EnvUnsupported }

// ClientSafe reports whether this mod may be shipped to a player in the client export.
func (h Hit) ClientSafe() bool { return h.ClientSide != EnvUnsupported }

// File is one downloadable artifact of a version.
type File struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
	SHA1     string `json:"sha1"`
	SHA512   string `json:"sha512"`
	Size     int64  `json:"size"`
	Primary  bool   `json:"primary"`
}

// Dep is one declared dependency of a version.
type Dep struct {
	ProjectID string `json:"projectId,omitempty"`
	VersionID string `json:"versionId,omitempty"`
	Type      string `json:"type"` // required | optional | incompatible | embedded
}

// Version is one published build of a project.
type Version struct {
	ID            string   `json:"id"`
	ProjectID     string   `json:"projectId"`
	Name          string   `json:"name"`
	VersionNumber string   `json:"versionNumber"`
	Files         []File   `json:"files"`
	Dependencies  []Dep    `json:"dependencies,omitempty"`
	GameVersions  []string `json:"gameVersions"`
	Loaders       []string `json:"loaders"`
}

// PrimaryFile picks the jar to install. Modrinth flags one file as primary; when it does not (some
// older versions ship only sources/javadoc alongside the build) the first .jar is the artifact.
// A version with neither is not installable, which the caller must handle rather than assume.
func (v Version) PrimaryFile() (File, bool) {
	for _, f := range v.Files {
		if f.Primary {
			return f, true
		}
	}
	for _, f := range v.Files {
		if strings.HasSuffix(strings.ToLower(f.Filename), ".jar") {
			return f, true
		}
	}
	return File{}, false
}

// ToMod projects a resolved version + project onto the store record. It is the single place where a
// Modrinth answer becomes hosuto state, so the environment fields cannot be lost or invented on the
// way in. ID and Added are the store's to assign.
func ToMod(v Version, h Hit) (store.Mod, error) {
	f, ok := v.PrimaryFile()
	if !ok {
		return store.Mod{}, fmt.Errorf("modrinth: version %s has no installable jar", v.ID)
	}
	name := h.Title
	if name == "" {
		name = v.Name
	}
	return store.Mod{
		Source:     "modrinth",
		ProjectID:  v.ProjectID,
		VersionID:  v.ID,
		Name:       name,
		Filename:   f.Filename,
		URL:        f.URL,
		SHA1:       f.SHA1,
		SHA512:     f.SHA512,
		Size:       f.Size,
		ClientSide: env(h.ClientSide),
		ServerSide: env(h.ServerSide),
	}, nil
}

// Client talks to one Modrinth instance. Safe for concurrent use.
type Client struct {
	base string
	ua   string
	hc   *http.Client // metadata calls
	dl   *http.Client // jar fetches; separate because its timeout must be much larger

	lim   *limiter
	cache *cache
}

// New builds a client. An empty baseURL means the public API; a nil hc means hosuto's defaults.
//
// userAgent must be the descriptive string Modrinth's terms require and comes from config; if the
// caller passes none we substitute a descriptive constant rather than let Go's default "Go-http-
// client/1.1" reach Modrinth, which is exactly what their policy exists to reject.
func New(baseURL, userAgent string, hc *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if userAgent == "" {
		userAgent = "hosuto (holistic services landscape)"
	}
	var tr http.RoundTripper
	if hc == nil {
		hc = &http.Client{Timeout: apiTimeout}
	} else {
		tr = hc.Transport
	}
	now := time.Now
	return &Client{
		base:  strings.TrimSuffix(baseURL, "/"),
		ua:    userAgent,
		hc:    hc,
		dl:    &http.Client{Transport: tr, Timeout: downloadTimeout},
		lim:   &limiter{tokens: rateBurst, now: now},
		cache: &cache{m: map[string]cacheEntry{}, now: now},
	}
}

// Search returns projects matching a free-text query, narrowed to installable mods for this server.
func (c *Client) Search(ctx context.Context, query, mcVersion, loader string, limit int) ([]Hit, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 { // Modrinth's ceiling; asking for more is an error, not a bigger page
		limit = 100
	}
	fs, err := Facets(mcVersion, loader)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("query", query)
	q.Set("facets", fs)
	q.Set("limit", fmt.Sprint(limit))

	var out struct {
		Hits []struct {
			ProjectID   string `json:"project_id"`
			Slug        string `json:"slug"`
			Title       string `json:"title"`
			Description string `json:"description"`
			IconURL     string `json:"icon_url"`
			ClientSide  string `json:"client_side"`
			ServerSide  string `json:"server_side"`
			Downloads   int    `json:"downloads"`
		} `json:"hits"`
	}
	if err := c.get(ctx, "/search", q, &out); err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(out.Hits))
	for _, h := range out.Hits {
		hits = append(hits, Hit{
			ProjectID: h.ProjectID, Slug: h.Slug, Title: h.Title, Description: h.Description,
			IconURL: h.IconURL, ClientSide: env(h.ClientSide), ServerSide: env(h.ServerSide),
			Downloads: h.Downloads,
		})
	}
	return hits, nil
}

// Project fetches one project by id or slug.
//
// The project document keys its identity as "id", while a search hit keys it as "project_id" — the
// same value under two names. Decoding both into Hit is deliberate: the callers (mod tab, export)
// need the environment pair and do not care which endpoint produced it.
func (c *Client) Project(ctx context.Context, id string) (Hit, error) {
	if id == "" {
		return Hit{}, fmt.Errorf("modrinth: empty project id")
	}
	var p struct {
		ID          string `json:"id"`
		Slug        string `json:"slug"`
		Title       string `json:"title"`
		Description string `json:"description"`
		IconURL     string `json:"icon_url"`
		ClientSide  string `json:"client_side"`
		ServerSide  string `json:"server_side"`
		Downloads   int    `json:"downloads"`
	}
	if err := c.get(ctx, "/project/"+url.PathEscape(id), nil, &p); err != nil {
		return Hit{}, err
	}
	return Hit{
		ProjectID: p.ID, Slug: p.Slug, Title: p.Title, Description: p.Description,
		IconURL: p.IconURL, ClientSide: env(p.ClientSide), ServerSide: env(p.ServerSide),
		Downloads: p.Downloads,
	}, nil
}

// Versions returns a project's versions for one game version + loader, newest first.
//
// The server-side filter is re-applied locally on top of the query parameters. Modrinth honours the
// parameters today, but installing a jar built for another loader bricks the server on next boot,
// and that is too expensive a failure to leave resting on the assumption that a remote filter ran.
func (c *Client) Versions(ctx context.Context, projectID, mcVersion, loader string) ([]Version, error) {
	if projectID == "" {
		return nil, fmt.Errorf("modrinth: empty project id")
	}
	q := url.Values{}
	if mcVersion != "" {
		b, err := json.Marshal([]string{mcVersion})
		if err != nil {
			return nil, err
		}
		q.Set("game_versions", string(b))
	}
	if modded(loader) {
		b, err := json.Marshal([]string{loader})
		if err != nil {
			return nil, err
		}
		q.Set("loaders", string(b))
	}

	var docs []versionDoc
	if err := c.get(ctx, "/project/"+url.PathEscape(projectID)+"/version", q, &docs); err != nil {
		return nil, err
	}
	return convert(docs, mcVersion, loader), nil
}

// Resolve picks the newest version of a project that matches this server and returns it with the
// project metadata — the version supplies the jar, the project supplies the environment pair. Both
// are needed to record a store.Mod, so they are fetched together and fail together.
func (c *Client) Resolve(ctx context.Context, projectID, mcVersion, loader string) (Version, Hit, error) {
	vs, err := c.Versions(ctx, projectID, mcVersion, loader)
	if err != nil {
		return Version{}, Hit{}, err
	}
	if len(vs) == 0 {
		return Version{}, Hit{}, fmt.Errorf("%w: %s on %s/%s", ErrNoVersion, projectID, mcVersion, loader)
	}
	h, err := c.Project(ctx, projectID)
	if err != nil {
		return Version{}, Hit{}, err
	}
	return vs[0], h, nil
}

// VersionsByHash resolves already-present jars (an upload, or a mods/ dir hosuto did not write) back
// to their Modrinth versions, keyed by lowercase sha1.
//
// This is the batch endpoint on purpose: the per-hash GET /version_file/{hash} is throttled hard
// enough that a 40-mod server would trip the rate limit resolving its own mod list.
func (c *Client) VersionsByHash(ctx context.Context, sha1s []string) (map[string]Version, error) {
	out := map[string]Version{}
	var batch []string
	for _, h := range sha1s {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			batch = append(batch, h)
		}
	}
	for len(batch) > 0 {
		n := min(hashBatch, len(batch))
		chunk := batch[:n]
		batch = batch[n:]

		body, err := json.Marshal(map[string]any{"hashes": chunk, "algorithm": "sha1"})
		if err != nil {
			return nil, err
		}
		var docs map[string]versionDoc
		if err := c.post(ctx, "/version_files", body, &docs); err != nil {
			return nil, err
		}
		for hash, d := range docs {
			// Unfiltered: the caller is identifying jars that already exist, not choosing new ones.
			if vs := convert([]versionDoc{d}, "", ""); len(vs) == 1 {
				out[strings.ToLower(hash)] = vs[0]
			}
		}
	}
	return out, nil
}

// Download fetches a file to dst, verifying every checksum Modrinth published for it.
//
// A file with no checksum is refused outright: an unverified jar is arbitrary remote code about to
// be run by the server, and "the CDN is probably fine" is not a security model. The bytes land in a
// temp file in dst's directory and are renamed only after they verify, so a truncated or tampered
// download can never appear at dst — the same atomic discipline the store uses for state.
func (c *Client) Download(ctx context.Context, f File, dst string) error {
	if f.URL == "" {
		return fmt.Errorf("modrinth: file %q has no url", f.Filename)
	}
	if f.SHA1 == "" && f.SHA512 == "" {
		return fmt.Errorf("modrinth: refusing to download %q: no checksum", f.Filename)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.ua)
	if err := c.lim.wait(ctx); err != nil {
		return err
	}
	resp, err := c.dl.Do(req)
	if err != nil {
		return fmt.Errorf("modrinth: download %s: %w", f.Filename, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("modrinth: download %s: %s", f.Filename, resp.Status)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".mod-*.part")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	h1, h5 := sha1.New(), sha512.New()
	n, err := io.Copy(io.MultiWriter(tmp, h1, h5), io.LimitReader(resp.Body, maxJarBytes+1))
	if err != nil {
		tmp.Close()
		return fmt.Errorf("modrinth: download %s: %w", f.Filename, err)
	}
	if n > maxJarBytes {
		tmp.Close()
		return fmt.Errorf("modrinth: %s exceeds %d bytes", f.Filename, int64(maxJarBytes))
	}
	if f.Size > 0 && n != f.Size {
		tmp.Close()
		return fmt.Errorf("modrinth: %s: got %d bytes, want %d", f.Filename, n, f.Size)
	}
	if f.SHA1 != "" && !strings.EqualFold(hex.EncodeToString(h1.Sum(nil)), f.SHA1) {
		tmp.Close()
		return fmt.Errorf("%w: %s sha1", ErrChecksum, f.Filename)
	}
	if f.SHA512 != "" && !strings.EqualFold(hex.EncodeToString(h5.Sum(nil)), f.SHA512) {
		tmp.Close()
		return fmt.Errorf("%w: %s sha512", ErrChecksum, f.Filename)
	}

	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// ── wire ──────────────────────────────────────────────────────────────────────────────

type versionDoc struct {
	ID            string    `json:"id"`
	ProjectID     string    `json:"project_id"`
	Name          string    `json:"name"`
	VersionNumber string    `json:"version_number"`
	DatePublished time.Time `json:"date_published"`
	GameVersions  []string  `json:"game_versions"`
	Loaders       []string  `json:"loaders"`
	Files         []struct {
		URL      string `json:"url"`
		Filename string `json:"filename"`
		Primary  bool   `json:"primary"`
		Size     int64  `json:"size"`
		Hashes   struct {
			SHA1   string `json:"sha1"`
			SHA512 string `json:"sha512"`
		} `json:"hashes"`
	} `json:"files"`
	Dependencies []struct {
		ProjectID      string `json:"project_id"` // null for a version-pinned dependency
		VersionID      string `json:"version_id"` // null for a project-level dependency
		DependencyType string `json:"dependency_type"`
	} `json:"dependencies"`
}

// convert decodes and orders versions newest-first, dropping any that does not actually match the
// requested game version and loader. Empty mcVersion/loader disable the respective filter.
func convert(docs []versionDoc, mcVersion, loader string) []Version {
	keep := make([]versionDoc, 0, len(docs))
	for _, d := range docs {
		if mcVersion != "" && !contains(d.GameVersions, mcVersion) {
			continue
		}
		if modded(loader) && !contains(d.Loaders, loader) {
			continue
		}
		keep = append(keep, d)
	}
	// Stable, so that versions published in the same second keep Modrinth's own order.
	sort.SliceStable(keep, func(i, j int) bool {
		return keep[i].DatePublished.After(keep[j].DatePublished)
	})

	out := make([]Version, 0, len(keep))
	for _, d := range keep {
		v := Version{
			ID: d.ID, ProjectID: d.ProjectID, Name: d.Name, VersionNumber: d.VersionNumber,
			GameVersions: d.GameVersions, Loaders: d.Loaders,
		}
		for _, f := range d.Files {
			v.Files = append(v.Files, File{
				URL: f.URL, Filename: f.Filename, SHA1: f.Hashes.SHA1, SHA512: f.Hashes.SHA512,
				Size: f.Size, Primary: f.Primary,
			})
		}
		for _, dep := range d.Dependencies {
			v.Dependencies = append(v.Dependencies, Dep{
				ProjectID: dep.ProjectID, VersionID: dep.VersionID, Type: dep.DependencyType,
			})
		}
		out = append(out, v)
	}
	return out
}

// Facets builds Modrinth's search facet parameter: a JSON array of arrays of strings, where the
// inner arrays are OR-ed and the outer array is AND-ed. It goes on the wire as URL-encoded JSON —
//
//	[["project_type:mod"],["versions:1.21.1"],["categories:fabric"]]
//
// Exported so the encoding has one definition and one test, rather than being re-derived by string
// concatenation at each call site (which is how the brackets and quotes get lost).
func Facets(mcVersion, loader string) (string, error) {
	groups := [][]string{{"project_type:mod"}}
	if mcVersion != "" {
		groups = append(groups, []string{"versions:" + mcVersion})
	}
	// Vanilla runs no mods, so it has no loader category; sending "categories:vanilla" would match
	// nothing and quietly return an empty search rather than an error.
	if modded(loader) {
		groups = append(groups, []string{"categories:" + loader})
	}
	b, err := json.Marshal(groups)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// env normalises Modrinth's environment field. Anything outside the vocabulary — including an
// absent field, a null, or a value Modrinth adds after this code was written — is "unknown".
// Guessing here is what this package exists to prevent.
func env(s string) string {
	switch s {
	case EnvRequired, EnvOptional, EnvUnsupported:
		return s
	default:
		return EnvUnknown
	}
}

// modded reports whether a loader has a Modrinth loader/category facet at all.
func modded(l string) bool { return l != "" && l != "vanilla" }

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// ── transport ─────────────────────────────────────────────────────────────────────────

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	if b, ok := c.cache.get(u); ok {
		return json.Unmarshal(b, out)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	b, err := c.do(req)
	if err != nil {
		return err
	}
	c.cache.put(u, b)
	return json.Unmarshal(b, out)
}

// post is never cached: its only caller resolves hashes, and a stale answer there would mislabel a
// jar that is sitting on disk right now.
func (c *Client) post(ctx context.Context, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	b, err := c.do(req)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func (c *Client) do(req *http.Request) ([]byte, error) {
	req.Header.Set("User-Agent", c.ua) // required by Modrinth's terms; see the package doc
	req.Header.Set("Accept", "application/json")
	if err := c.lim.wait(req.Context()); err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("modrinth: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("modrinth: %s %s: %w", req.Method, req.URL.Path, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", ErrNotFound, req.URL.Path)
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("%w (retry after %ss)", ErrRateLimited, resp.Header.Get("Retry-After"))
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("modrinth: %s %s: %s: %s", req.Method, req.URL.Path, resp.Status, snippet(body))
	}
	return body, nil
}

// snippet keeps an upstream error body out of the log in full while preserving its point.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// ── rate limit ────────────────────────────────────────────────────────────────────────

// limiter is a token bucket sized to Modrinth's 300 req/min. Tokens are allowed to go negative:
// that is the queue, and it makes each waiter wait for its own token rather than all of them waking
// at once and re-colliding. A cancelled wait does not refund its token — under a rate limit, the
// conservative error is to under-send.
type limiter struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	now    func() time.Time // injectable for tests
}

func (l *limiter) wait(ctx context.Context) error {
	l.mu.Lock()
	now := l.now()
	if l.last.IsZero() {
		l.last = now
	}
	l.tokens += now.Sub(l.last).Seconds() * ratePerSec
	if l.tokens > rateBurst {
		l.tokens = rateBurst
	}
	l.last = now
	l.tokens--
	deficit := l.tokens
	l.mu.Unlock()

	if deficit >= 0 {
		return nil
	}
	t := time.NewTimer(time.Duration(-deficit / ratePerSec * float64(time.Second)))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ── cache ─────────────────────────────────────────────────────────────────────────────

type cacheEntry struct {
	body []byte
	at   time.Time
}

// cache is a bounded, short-TTL store of raw GET bodies keyed by full URL. Only 200s are cached: an
// error must be re-attempted, never remembered.
type cache struct {
	mu  sync.Mutex
	m   map[string]cacheEntry
	now func() time.Time
}

func (c *cache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || c.now().Sub(e.at) >= cacheTTL {
		return nil, false
	}
	return e.body, true
}

func (c *cache) put(key string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= cacheMax {
		// Drop what has expired; if that frees nothing, drop everything. A wrong eviction costs one
		// request, so the simple policy is the right one — an LRU here would be unearned machinery.
		now := c.now()
		for k, e := range c.m {
			if now.Sub(e.at) >= cacheTTL {
				delete(c.m, k)
			}
		}
		if len(c.m) >= cacheMax {
			clear(c.m)
		}
	}
	c.m[key] = cacheEntry{body: body, at: c.now()}
}
