// Package archive is hosuto's zip boundary: it unpacks an archive somebody else produced into a
// server tree, and packs a server tree back into a template payload.
//
// Reading an archive from outside is the dangerous direction, and every rule here exists because a
// zip is a list of NAMES chosen by whoever built it, not a description of a directory:
//
//   - Zip slip: an entry named "../../etc/holistic/permissions.d/hosuto.json" writes outside the
//     destination if the name is joined naively. Every name is cleaned and re-checked to still fall
//     under dest, and anything absolute, rooted or containing ".." is refused outright.
//   - Symlinks: a zip may carry one. Unpacking "mods -> /etc/holistic" and then writing through it
//     hands the archive author whatever the daemon can reach — the same attack files.Tree exists to
//     stop. Symlinks (and every other non-regular entry) are refused, never followed, never created.
//   - Zip bombs: the header's uncompressed size is a claim, not a fact. Limits bound the entry
//     count, the per-file size and the TOTAL bytes actually written, enforced on the copy itself
//     rather than on the declared size, so a lying header runs into the same ceiling.
//
// The modes are the tree's, not the archive's: a server directory is 2770 hs-<slug>:hosuto with a
// default ACL, so directories are created 0770 and files 0660 and the ACL does the rest. Honouring
// the mode bits in the zip would produce a 0644 file the game cannot write and a world that fails to
// save hours later, which is exactly the kind of failure this package must not create.
package archive

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

var (
	// ErrUnsafe is returned for an entry that would escape the destination or is not a regular file.
	ErrUnsafe = errors.New("archive: unsafe entry")
	// ErrTooLarge is returned when an archive exceeds the caller's limits.
	ErrTooLarge = errors.New("archive: too large")
)

// Limits bound what an extraction may cost. The zero value means DefaultLimits — a caller that
// forgets to set them gets the safe ones rather than none.
type Limits struct {
	MaxEntries int   // number of members
	MaxFile    int64 // bytes of any single member, after decompression
	MaxTotal   int64 // bytes written in total, after decompression
}

// DefaultLimits is sized for a real Minecraft server: a big modpack world with a few hundred mods and
// a region folder is comfortably inside it, while a bomb is not.
var DefaultLimits = Limits{
	MaxEntries: 200_000,
	MaxFile:    8 << 30,  // 8 GiB — a single large region/database file
	MaxTotal:   64 << 30, // 64 GiB
}

func (l Limits) orDefault() Limits {
	if l.MaxEntries <= 0 {
		l.MaxEntries = DefaultLimits.MaxEntries
	}
	if l.MaxFile <= 0 {
		l.MaxFile = DefaultLimits.MaxFile
	}
	if l.MaxTotal <= 0 {
		l.MaxTotal = DefaultLimits.MaxTotal
	}
	return l
}

// Result reports what an extraction actually wrote.
type Result struct {
	Files   int
	Dirs    int
	Bytes   int64
	Skipped []string // entries refused, for the migration report — never silently dropped
	Root    string   // the wrapper directory that was stripped, "" if none
}

// Extract unpacks the zip at src into dest, which must already exist. onBytes, when given, is called
// with each chunk written so a caller can drive a progress bar; it is display only, and nothing about
// the safety limits depends on it.
//
// A wrapper directory is stripped when the archive is a folder rather than its contents — see
// wrapperDir. This is deliberate leniency: "zip up your server" produces both shapes depending on
// the operating system and the person, and refusing one of them would fail the migration for a
// reason the user cannot see and did not choose.
func Extract(src, dest string, lim Limits, onBytes func(int64)) (Result, error) {
	lim = lim.orDefault()

	zr, err := zip.OpenReader(src)
	if err != nil {
		return Result{}, fmt.Errorf("archive: open: %w", err)
	}
	defer zr.Close()

	if len(zr.File) > lim.MaxEntries {
		return Result{}, fmt.Errorf("%w: %d entries", ErrTooLarge, len(zr.File))
	}

	realDest, err := filepath.EvalSymlinks(dest)
	if err != nil {
		return Result{}, err
	}

	var res Result
	res.Root = wrapperDir(zr.File)

	for _, f := range zr.File {
		name, ok := safeName(f.Name, res.Root)
		if !ok {
			if n := strings.TrimSpace(f.Name); n != "" {
				res.Skipped = append(res.Skipped, n)
			}
			continue
		}
		if name == "" {
			continue // the wrapper directory itself
		}
		abs := filepath.Join(realDest, filepath.FromSlash(name))
		// Belt and braces: Join+Clean already collapsed the path, so this can only fail if realDest
		// itself is odd — but the check costs nothing and the failure it catches is total.
		if !within(realDest, abs) {
			res.Skipped = append(res.Skipped, f.Name)
			continue
		}

		mode := f.Mode()
		switch {
		case f.FileInfo().IsDir():
			if err := os.MkdirAll(abs, 0o770); err != nil {
				return res, err
			}
			res.Dirs++
			continue
		case !mode.IsRegular():
			// Symlinks, devices, fifos, sockets. Recorded, never created.
			res.Skipped = append(res.Skipped, f.Name)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(abs), 0o770); err != nil {
			return res, err
		}
		n, err := writeFile(f, abs, lim.MaxFile, lim.MaxTotal-res.Bytes)
		if err != nil {
			return res, err
		}
		res.Files++
		res.Bytes += n
		if onBytes != nil {
			onBytes(n)
		}
		if !f.Modified.IsZero() {
			_ = os.Chtimes(abs, f.Modified, f.Modified)
		}
	}
	return res, nil
}

// writeFile copies one member to dst, bounded by the per-file and remaining-total budgets.
//
// The budget is enforced with a LimitReader set one byte past the ceiling: if that extra byte
// arrives, the member is over budget regardless of what its header promised. Nothing is trusted
// except the bytes that actually came out of the decompressor.
func writeFile(f *zip.File, dst string, maxFile, remaining int64) (int64, error) {
	budget := maxFile
	if remaining < budget {
		budget = remaining
	}
	if budget < 0 {
		return 0, fmt.Errorf("%w: total size budget exhausted", ErrTooLarge)
	}

	rc, err := f.Open()
	if err != nil {
		return 0, fmt.Errorf("archive: %s: %w", f.Name, err)
	}
	defer rc.Close()

	// 0660: the group is hosuto (setgid on the tree) and the default ACL grants the run account rwx,
	// so the game can write what it must own. See the package comment.
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o660)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(rc, budget+1))
	if err != nil {
		out.Close()
		_ = os.Remove(dst)
		return 0, fmt.Errorf("archive: %s: %w", f.Name, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return 0, err
	}
	if n > budget {
		_ = os.Remove(dst)
		return 0, fmt.Errorf("%w: %s", ErrTooLarge, f.Name)
	}
	return n, nil
}

// safeName turns an archive member name into a clean relative slash path under dest, or reports that
// it is not usable. It strips the wrapper prefix when there is one.
//
// Everything about a member name is attacker-controlled, so the rules are absolute rather than
// heuristic: no absolute path, no drive letter or UNC prefix, no "..", no NUL. What survives is a
// path built only from ordinary components.
func safeName(name, wrapper string) (string, bool) {
	if strings.ContainsRune(name, 0) {
		return "", false
	}
	// Windows-built archives use backslashes even though the spec says forward. Normalising first
	// means "a\..\..\b" cannot slip past a separator-blind check.
	n := strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(n, "/") || strings.Contains(n, ":") {
		return "", false
	}
	n = path.Clean(n)
	if n == "." {
		return "", true // the archive root itself: nothing to write, not an error
	}
	if n == ".." || strings.HasPrefix(n, "../") {
		return "", false
	}
	if wrapper != "" {
		switch {
		case n == wrapper:
			return "", true // the wrapper directory entry itself
		case strings.HasPrefix(n, wrapper+"/"):
			n = strings.TrimPrefix(n, wrapper+"/")
		default:
			// Outside the wrapper we decided to strip. Keeping it would scatter files next to the
			// tree we are unpacking, so it is refused and reported.
			return "", false
		}
	}
	if n == "" || n == "." {
		return "", true
	}
	return n, true
}

// wrapperDir reports the single top-level directory to strip, or "" to unpack as-is.
//
// The rule is evidence-based rather than "always strip a lone folder": strip only when the archive
// root holds NO server marker while exactly one top-level directory does. So a correctly-rooted
// archive is never mangled, a zipped-up folder is handled, and an archive that is neither is left
// alone rather than guessed at.
func wrapperDir(files []*zip.File) string {
	tops := map[string]bool{}
	rootMarker := false
	for _, f := range files {
		n, ok := safeName(f.Name, "")
		if !ok || n == "" {
			continue
		}
		first, _, nested := strings.Cut(n, "/")
		if !nested {
			// A file directly in the archive root.
			if isMarker(first, !f.FileInfo().IsDir()) {
				rootMarker = true
			}
			if f.FileInfo().IsDir() {
				tops[first] = true
			}
			continue
		}
		tops[first] = true
	}
	if rootMarker || len(tops) != 1 {
		return ""
	}
	var only string
	for k := range tops {
		only = k
	}
	// The lone top-level directory must itself look like a server, else stripping it would be a guess.
	for _, f := range files {
		n, ok := safeName(f.Name, "")
		if !ok || !strings.HasPrefix(n, only+"/") {
			continue
		}
		inner := strings.TrimPrefix(n, only+"/")
		if !strings.Contains(inner, "/") && isMarker(inner, !f.FileInfo().IsDir()) {
			return only
		}
		if head, _, nested := strings.Cut(inner, "/"); nested && isMarker(head, false) {
			return only
		}
	}
	return ""
}

// isMarker reports whether a top-level name is characteristic of a Minecraft server root.
func isMarker(name string, isFile bool) bool {
	if isFile {
		switch name {
		case "server.properties", "eula.txt", "ops.json", "whitelist.json", "banned-players.json",
			"usercache.json", "user_jvm_args.txt", "fabric-server-launcher.properties",
			"version_history.json", "bukkit.yml", "spigot.yml", "paper.yml", "start.sh", "run.sh":
			return true
		}
		return strings.HasSuffix(strings.ToLower(name), ".jar")
	}
	switch name {
	case "world", "mods", "plugins", "config", "libraries", "versions", "logs", "cache", "defaultconfigs":
		return true
	}
	return strings.HasPrefix(name, "world") // world_nether, world_the_end, and renamed level dirs
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ── writing ───────────────────────────────────────────────────────────────────────────

// Create packs srcDir into w. skip decides, per relative slash path, what stays out — the caller
// owns that policy because "what belongs in a template" is a product question, not an archive one.
//
// extra adds synthesized members that are not files on disk. It exists so a caller can pack a
// SANITISED version of a file it also skipped: a template must carry a server's gameplay settings
// without carrying its rcon password, and the only way to do both is to skip the real
// server.properties and add a filtered one under the same name.
//
// Symlinks are skipped rather than followed: a server tree is writable by the run account, so a
// symlink in it may point anywhere the daemon can read, and following one would pack that content
// into a template any other user may instantiate.
func Create(w io.Writer, srcDir string, skip func(rel string, d fs.DirEntry) bool, extra map[string][]byte) error {
	zw := zip.NewWriter(w)
	root, err := filepath.EvalSymlinks(srcDir)
	if err != nil {
		return err
	}
	for name, body := range extra {
		f, err := zw.Create(name)
		if err != nil {
			_ = zw.Close()
			return err
		}
		if _, err := f.Write(body); err != nil {
			_ = zw.Close()
			return err
		}
	}

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A file that vanished under a running server is normal, not fatal.
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if skip != nil && skip(rel, d) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			_, err := zw.Create(rel + "/")
			return err
		}
		if !d.Type().IsRegular() {
			return nil // symlinks, sockets, fifos: never packed
		}
		return addFile(zw, p, rel, d)
	})
	if walkErr != nil {
		_ = zw.Close()
		return walkErr
	}
	return zw.Close()
}

func addFile(zw *zip.Writer, abs, rel string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	f, err := os.Open(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
			return nil
		}
		return err
	}
	defer f.Close()

	hdr := &zip.FileHeader{Name: rel, Method: zip.Deflate, Modified: info.ModTime()}
	// Region files and world databases are already compressed; spending CPU on them buys nothing.
	if precompressed(rel) {
		hdr.Method = zip.Store
	}
	out, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, f)
	return err
}

func precompressed(rel string) bool {
	switch strings.ToLower(path.Ext(rel)) {
	case ".jar", ".zip", ".gz", ".mca", ".mcr", ".png", ".ogg", ".xz", ".7z":
		return true
	}
	return false
}
