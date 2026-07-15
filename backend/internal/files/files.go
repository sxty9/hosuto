// Package files is hosuto's confined view of one server's on-disk tree, for the "Spieledateien" tab.
//
// It exposes the same operations the holistic Files service does — list, download, preview, upload,
// mkdir, rename, move, copy, delete — but every one is locked inside a single server directory.
//
// The confinement is the whole point, and it is not cosmetic. A server tree is owned by the player
// (the hs-<slug> run account), so the player can create anything in it — including a SYMLINK to
// /etc/holistic/jwt-secret. hosutod runs as the `hosuto` user, which is in the `holistic` group and
// therefore CAN read that secret. So a browser that naively followed a symlink would hand a member
// the shared session key and let them forge a session as anyone. Every path here is therefore
// resolved through EvalSymlinks and checked to still fall within the server root; symlinks are never
// listed and never traversed.
//
// hosutod reaches the files directly (no privileged wrapper): the tree is mode 2770 hs-<slug>:hosuto
// with a default ACL, and hosutod is in the `hosuto` group, so group access covers read, list and
// directory writes. Content it cannot open for writing (a 0644 file the game owns) it never edits in
// place — uploads land via a temp file renamed over the target, which only needs directory write.
package files

import (
	"errors"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RootKey is the single virtual root every path hangs under: "server/…". The holistic Files UI keys
// its breadcrumb and navigation on the first path segment, so hosuto presents exactly one root.
const RootKey = "server"

var (
	// ErrNotFound is returned for a path that does not exist (or resolves outside the tree).
	ErrNotFound = errors.New("not found")
	// ErrDenied is returned for a path that escapes the server root, or an illegal name.
	ErrDenied = errors.New("denied")
	// ErrExists is returned when a create would clobber an existing entry that must not be replaced.
	ErrExists = errors.New("already exists")
	// ErrInvalid is returned for a malformed request (bad name, root operation).
	ErrInvalid = errors.New("invalid")
)

// Entry is one file or directory, in the shape the holistic Files UI (@holistic/ui FileEntry) parses.
type Entry struct {
	Name        string `json:"name"`
	Path        string `json:"path"` // virtual, e.g. "server/mods/sodium.jar"
	Kind        string `json:"kind"` // "file" | "dir"
	Size        int64  `json:"size"`
	MTime       int64  `json:"mtime"` // epoch ms
	Mime        string `json:"mime,omitempty"`
	Viewer      string `json:"viewer,omitempty"` // image|text|markdown|audio|video|pdf, or ""
	Permissions string `json:"permissions,omitempty"`
}

// Root is the "FileRoot" the UI lists as a top-level location.
type Root struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Writable bool   `json:"writable"`
}

// Tree is a confined filesystem rooted at one server directory.
type Tree struct {
	root     string // absolute server directory as given
	realRoot string // EvalSymlinks(root), the canonical form every access is checked against
}

// Open builds a Tree for the given absolute server directory.
func Open(root string) (*Tree, error) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	return &Tree{root: root, realRoot: real}, nil
}

// Roots returns the single virtual root for this server.
func (t *Tree) Roots() []Root {
	return []Root{{Key: RootKey, Label: "Server", Writable: true}}
}

// resolveExisting maps a virtual path to a real absolute path that MUST already exist and MUST fall
// inside the server root after all symlinks are resolved. This is the gate for read/list/delete/rename
// and for the source of a move/copy.
func (t *Tree) resolveExisting(vpath string) (string, error) {
	abs, err := t.lexical(vpath)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNotFound
		}
		return "", err
	}
	if !t.within(real) {
		return "", ErrDenied
	}
	return real, nil
}

// resolveParent maps a virtual DIRECTORY path to a real path for creating a NEW child called `name`
// inside it. The parent must exist and be inside the root; the child must not already be something we
// would clobber blindly. This is the gate for mkdir, upload and the destination of a move/copy.
func (t *Tree) resolveParent(vDir, name string) (string, error) {
	if !validName(name) {
		return "", ErrInvalid
	}
	dir, err := t.resolveExisting(vDir)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return "", ErrInvalid
	}
	child := filepath.Join(dir, name)
	// The parent is already confirmed inside the root and symlink-free; Join with a validated,
	// separator-free name cannot escape it. A symlink AT the child is caught on the next access.
	if !t.within(child) {
		return "", ErrDenied
	}
	return child, nil
}

// lexical turns a virtual path into a candidate absolute path WITHOUT touching the filesystem. It
// enforces the root key and rejects any traversal, so a caller-supplied "server/../../etc" never even
// reaches a stat.
func (t *Tree) lexical(vpath string) (string, error) {
	vpath = strings.Trim(strings.TrimSpace(vpath), "/")
	if vpath == "" {
		return "", ErrInvalid
	}
	parts := strings.Split(vpath, "/")
	if parts[0] != RootKey {
		return "", ErrDenied
	}
	rel := filepath.Clean("/" + strings.Join(parts[1:], "/")) // leading slash anchors Clean, kills ".."
	if rel == "/" {
		return t.root, nil
	}
	if strings.Contains(rel, "..") {
		return "", ErrDenied
	}
	return filepath.Join(t.root, rel), nil
}

// within reports whether a real path is the root itself or strictly under it.
func (t *Tree) within(real string) bool {
	if real == t.realRoot {
		return true
	}
	// A path that shares a prefix but not a boundary ("/srv/root-evil" vs "/srv/root") is NOT inside.
	rel, err := filepath.Rel(t.realRoot, real)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// vpathOf turns a real path back into its virtual form for an Entry.
func (t *Tree) vpathOf(real string) string {
	rel, err := filepath.Rel(t.realRoot, real)
	if err != nil || rel == "." {
		return RootKey
	}
	return RootKey + "/" + filepath.ToSlash(rel)
}

func (t *Tree) entry(real string, fi os.FileInfo) Entry {
	e := Entry{
		Name:        fi.Name(),
		Path:        t.vpathOf(real),
		MTime:       fi.ModTime().UnixMilli(),
		Permissions: fi.Mode().Perm().String(),
	}
	if fi.IsDir() {
		e.Kind = "dir"
		return e
	}
	e.Kind = "file"
	e.Size = fi.Size()
	e.Mime, e.Viewer = classify(fi.Name())
	return e
}

// List returns the entries of a directory, sorted dirs-first then by name. Symlinks are omitted:
// they are never navigable in this tree (see the package doc), so showing them would only invite a
// click that resolveExisting is going to refuse anyway.
func (t *Tree) List(vpath string) ([]Entry, error) {
	real, err := t.resolveExisting(vpath)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, ErrInvalid
	}
	des, err := os.ReadDir(real)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(des))
	for _, de := range des {
		if de.Type()&os.ModeSymlink != 0 {
			continue // never traversed, so never listed
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		out = append(out, t.entry(filepath.Join(real, de.Name()), info))
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Kind == "dir") != (out[j].Kind == "dir") {
			return out[i].Kind == "dir"
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// Stat returns one entry.
func (t *Tree) Stat(vpath string) (Entry, error) {
	real, err := t.resolveExisting(vpath)
	if err != nil {
		return Entry{}, err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return Entry{}, err
	}
	return t.entry(real, fi), nil
}

// OpenFile opens a regular file for reading (download/raw). The caller closes it.
func (t *Tree) OpenFile(vpath string) (*os.File, Entry, error) {
	real, err := t.resolveExisting(vpath)
	if err != nil {
		return nil, Entry{}, err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return nil, Entry{}, err
	}
	if fi.IsDir() {
		return nil, Entry{}, ErrInvalid
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, Entry{}, err
	}
	return f, t.entry(real, fi), nil
}

// ReadText reads up to max bytes of a file as UTF-8 text for the preview. It reports truncation so
// the viewer can say "showing the first N".
func (t *Tree) ReadText(vpath string, max int64) (string, bool, error) {
	f, _, err := t.OpenFile(vpath)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return "", false, err
	}
	truncated := int64(len(b)) > max
	if truncated {
		b = b[:max]
	}
	return string(b), truncated, nil
}

// Mkdir creates a new subdirectory. The setgid bit + default ACL on the tree carry ownership and the
// run account's access down to it, so no chown is needed.
func (t *Tree) Mkdir(vDir, name string) error {
	child, err := t.resolveParent(vDir, name)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(child); err == nil {
		return ErrExists
	}
	return os.Mkdir(child, 0o770)
}

// Rename renames an entry in place (same directory).
func (t *Tree) Rename(vpath, newName string) error {
	real, err := t.resolveExisting(vpath)
	if err != nil {
		return err
	}
	if real == t.realRoot {
		return ErrInvalid // the server directory itself is not renameable
	}
	if !validName(newName) {
		return ErrInvalid
	}
	dst := filepath.Join(filepath.Dir(real), newName)
	if !t.within(dst) {
		return ErrDenied
	}
	if _, err := os.Lstat(dst); err == nil {
		return ErrExists
	}
	return os.Rename(real, dst)
}

// Move moves an entry into another directory (both inside the tree).
func (t *Tree) Move(vsrc, vDstDir string) error {
	src, err := t.resolveExisting(vsrc)
	if err != nil {
		return err
	}
	if src == t.realRoot {
		return ErrInvalid
	}
	dst, err := t.resolveParent(vDstDir, filepath.Base(src))
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		return ErrExists
	}
	return os.Rename(src, dst)
}

// Copy copies a file or directory tree into another directory.
func (t *Tree) Copy(vsrc, vDstDir string) error {
	src, err := t.resolveExisting(vsrc)
	if err != nil {
		return err
	}
	dst, err := t.resolveParent(vDstDir, filepath.Base(src))
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		return ErrExists
	}
	return copyTree(src, dst)
}

// Delete removes an entry. A directory needs recursive=true; the server root can never be deleted.
func (t *Tree) Delete(vpath string, recursive bool) error {
	real, err := t.resolveExisting(vpath)
	if err != nil {
		return err
	}
	if real == t.realRoot {
		return ErrInvalid
	}
	fi, err := os.Stat(real)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		if !recursive {
			return ErrInvalid
		}
		return os.RemoveAll(real)
	}
	return os.Remove(real)
}

// Save streams an upload into vDstDir/name. It writes a temp file in the same directory and renames
// it over the target, so it can replace even a file the game owns (rename needs only directory
// write, which hosutod has through the group) and a failed upload never leaves a half-written file.
func (t *Tree) Save(vDstDir, name string, r io.Reader) error {
	dst, err := t.resolveParent(vDstDir, name)
	if err != nil {
		return err
	}
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmpName, 0o660)
	return os.Rename(tmpName, dst)
}

// ── helpers ───────────────────────────────────────────────────────────────────────────

// validName rejects anything that is not a single, safe path component.
func validName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 {
		return false
	}
	return !strings.ContainsAny(name, "/\x00")
}

func copyTree(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	// A symlink anywhere in the source subtree would let a copy smuggle an out-of-tree target back
	// inside as a real file. Refuse it rather than following it.
	if fi.Mode()&os.ModeSymlink != 0 {
		return ErrDenied
	}
	if fi.IsDir() {
		if err := os.Mkdir(dst, 0o770); err != nil {
			return err
		}
		des, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, de := range des {
			if err := copyTree(filepath.Join(src, de.Name()), filepath.Join(dst, de.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o660)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// textExt is the set of extensions hosuto previews as text/markdown. Minecraft's own config surface
// is almost entirely one of these, which is the point of the tab.
var textExt = map[string]string{
	".txt": "text", ".log": "text", ".properties": "text", ".json": "text", ".json5": "text",
	".yml": "text", ".yaml": "text", ".toml": "text", ".cfg": "text", ".conf": "text",
	".ini": "text", ".sh": "text", ".bat": "text", ".csv": "text", ".tsv": "text",
	".mcmeta": "text", ".mcfunction": "text", ".nbtx": "text", ".xml": "text", ".html": "text",
	".js": "text", ".ts": "text", ".java": "text", ".kt": "text", ".gradle": "text",
	".md": "markdown", ".markdown": "markdown",
}

var mediaExt = map[string]string{
	".png": "image", ".jpg": "image", ".jpeg": "image", ".gif": "image", ".webp": "image", ".bmp": "image",
	".ogg": "audio", ".mp3": "audio", ".wav": "audio", ".flac": "audio",
	".mp4": "video", ".webm": "video", ".mkv": "video",
	".pdf": "pdf",
}

// classify returns a best-effort mime type and the SDK viewer kind for a filename.
func classify(name string) (mimeType, viewer string) {
	ext := strings.ToLower(filepath.Ext(name))
	if v, ok := textExt[ext]; ok {
		viewer = v
	} else if v, ok := mediaExt[ext]; ok {
		viewer = v
	}
	mimeType = mime.TypeByExtension(ext)
	if mimeType == "" && (viewer == "text" || viewer == "markdown") {
		mimeType = "text/plain; charset=utf-8"
	}
	return mimeType, viewer
}

// DownloadName is a filename safe to drop into a Content-Disposition header (no quotes, backslashes
// or control characters that could break out of the quoted value).
func DownloadName(e Entry) string {
	n := strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 {
			return '_'
		}
		return r
	}, e.Name)
	if n == "" {
		return "download"
	}
	return n
}
