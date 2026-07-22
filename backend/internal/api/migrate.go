// This file holds the three ways a server comes into existence beyond a blank one: from a TEMPLATE
// (a recipe saved off an existing server) and from a MIGRATION (an archive somebody uploaded, or a
// tree pulled off a foreign host over FTP).
//
// The important idea is that all three end at the same place. A migration that only moved files
// would leave hosuto with a directory it does not understand: the Modding tab empty while forty jars
// sit in mods/, the Mitglieder tab empty while whitelist.json is full, and hosuto about to install
// the wrong Minecraft over a world that cannot open it. So every path here runs the same three
// steps — lay the files down, READ what they are (package detect), then commit that reading into
// hosuto's own state — and the tabs are filled because the state behind them is, not because each
// tab was special-cased.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"hosuto/internal/archive"
	"hosuto/internal/auth"
	"hosuto/internal/detect"
	"hosuto/internal/ftp"
	"hosuto/internal/jobs"
	"hosuto/internal/mcfiles"
	"hosuto/internal/modrinth"
	"hosuto/internal/rights"
	"hosuto/internal/runtime"
	"hosuto/internal/store"
)

// maxUpload bounds an uploaded archive. A migrated modpack server with years of world is tens of
// gigabytes; past this it is not a server, and the daemon's disk is shared with the rest of the
// landscape.
const maxUpload = 64 << 30

// ── templates ─────────────────────────────────────────────────────────────────────────

// templateDir is where template payloads live: beside the state file, like the chats and the MCP
// tokens. They are the daemon's own data, not a server's, so they are deliberately NOT under the
// servers root where a run account could reach them.
func (s *Server) templateDir() string { return filepath.Join(s.dataDir, "templates") }

func (s *Server) templatePath(id string) string {
	return filepath.Join(s.templateDir(), id+".zip")
}

// visibleTemplate resolves a template the caller may use. Templates are owned exactly like servers:
// the creator and an admin. A template carries the source server's config files, which can hold a
// plugin's database password — so it inherits the server's confidentiality, not a laxer one.
func (s *Server) visibleTemplate(id string, u *auth.User) (store.Template, bool) {
	t, ok := s.st.Template(id)
	if !ok {
		return store.Template{}, false
	}
	return t, t.Owner == u.Username || u.Can(rights.GroupAdmin)
}

func (s *Server) listTemplates(w http.ResponseWriter, _ *http.Request, u *auth.User) {
	out := []store.Template{}
	for _, t := range s.st.Templates() {
		if t.Owner == u.Username || u.Can(rights.GroupAdmin) {
			out = append(out, t)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

// createTemplate saves a server as a reusable recipe. It runs as a job: packing a world is minutes
// of I/O, and holding an HTTP request open for it would put a proxy timeout between the user and
// their template.
func (s *Server) createTemplate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var body struct {
		ServerID     string `json:"serverId"`
		Name         string `json:"name"`
		IncludeWorld bool   `json:"includeWorld"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "Malformed request")
		return
	}
	srv, ok := s.findServer(body.ServerID)
	if !ok || !(srv.Owner == u.Username || u.Can(rights.GroupAdmin)) {
		writeErr(w, http.StatusNotFound, "No such server")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = srv.Name
	}
	if err := os.MkdirAll(s.templateDir(), 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create the template folder")
		return
	}

	job := s.jobs.Start("template", u.Username, "packing", func(h *jobs.Handle) error {
		return s.packTemplate(h, srv, name, body.IncludeWorld)
	})
	writeJSON(w, http.StatusOK, job)
}

// packTemplate writes the payload, then records the template.
//
// The payload is written to a temp file and renamed onto the id only after the record exists, so the
// two can never disagree: a crash leaves either nothing or a complete pair, never a record pointing
// at a half-written zip that would instantiate a broken server.
func (s *Server) packTemplate(h *jobs.Handle, srv store.Server, name string, includeWorld bool) error {
	dir := runtime.Dir(srv.Owner, srv.Slug)
	if _, err := os.Stat(dir); err != nil {
		return errors.New("this server has no files on disk yet")
	}

	tmp, err := os.CreateTemp(s.templateDir(), ".tpl-*.part")
	if err != nil {
		return errors.New("could not create the template file")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename succeeds

	// server.properties is packed as a SANITISED copy: the gameplay settings are exactly what makes a
	// template worth having (difficulty, gamemode, view-distance, the level seed), while the rcon
	// password and the port belong to the server it came from and to nothing else.
	extra := map[string][]byte{}
	if props, err := mcfiles.ReadProps(filepath.Join(dir, "server.properties")); err == nil {
		if b := renderTemplateProps(props); b != nil {
			extra["server.properties"] = b
		}
	}

	h.Message(srv.Name)
	err = archive.Create(tmp, dir, templateSkip(includeWorld), extra)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("could not pack the server: %w", err)
	}
	size := int64(0)
	if fi, err := os.Stat(tmpName); err == nil {
		size = fi.Size()
	}

	h.Phase("saving")
	t, err := s.st.CreateTemplate(store.Template{
		Name: name, Owner: srv.Owner, MCVersion: srv.MCVersion, Loader: srv.Loader,
		LoaderVersion: srv.LoaderVersion, HeapMB: srv.HeapMB, JoinPolicy: srv.JoinPolicy,
		Mods: srv.Mods, IncludeWorld: includeWorld, Size: size, SourceSlug: srv.Slug,
	})
	if err != nil {
		return errors.New("could not save the template")
	}
	if err := os.Rename(tmpName, s.templatePath(t.ID)); err != nil {
		_ = s.st.DeleteTemplate(t.ID)
		return errors.New("could not store the template")
	}
	h.Note(fmt.Sprintf("Saved %q.", t.Name))
	return nil
}

// templateSkip decides what a template does NOT carry.
//
// Three groups come out. Everything hosuto REGENERATES (the launch line, the lists, the accepted
// EULA) would otherwise be restored stale over the top of the new server's own. Everything
// DERIVED — libraries/, versions/, the server jars, caches, logs — is re-downloaded by the installer
// and would only make the payload enormous and the template bound to one loader build. And the
// world, unless the creator asked for a clone.
func templateSkip(includeWorld bool) func(string, fs.DirEntry) bool {
	return func(rel string, d fs.DirEntry) bool {
		top, _, _ := strings.Cut(rel, "/")
		if d.IsDir() {
			switch top {
			case "logs", "crash-reports", "cache", "libraries", "versions", "debug", ".fabric", ".mixin.out":
				return true
			}
			if !includeWorld && isWorldDir(top) {
				return true
			}
			return false
		}
		if !strings.Contains(rel, "/") {
			switch rel {
			// Regenerated by hosuto on every provision; a restored copy would be a stale lie.
			case "exec.argv", "eula.txt", "server.properties", "whitelist.json", "ops.json",
				"banned-players.json", "banned-ips.json", "usercache.json", "usernamecache.json",
				"session.lock", "user_jvm_args.txt", "version_history.json",
				"fabric-server-launcher.properties":
				return true
			}
			// The server jars are the installer's job, and they are the bulk of a payload.
			if strings.HasSuffix(strings.ToLower(rel), ".jar") {
				return true
			}
		}
		return false
	}
}

// isWorldDir reports whether a top-level directory is a world. level-name is configurable, so the
// prefix match catches a renamed one along with the nether and the end.
func isWorldDir(name string) bool {
	return name == "world" || strings.HasPrefix(name, "world_") || name == "DIM1" || name == "DIM-1"
}

// templateOwnedKeys are the server.properties keys a template must not carry: they belong to the
// specific server they came from, and hosuto rewrites them anyway.
var templateOwnedKeys = map[string]bool{
	"rcon.password": true, "rcon.port": true, "enable-rcon": true, "server-port": true,
	"server-ip": true, "motd": true, "query.port": true, "enable-query": true,
	"broadcast-rcon-to-ops": true, "white-list": true, "enforce-whitelist": true,
}

func renderTemplateProps(p mcfiles.Props) []byte {
	out := mcfiles.Props{}
	for k, v := range p {
		if !templateOwnedKeys[k] {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	var b strings.Builder
	// Written by hand rather than through mcfiles.WriteProps, which writes to a path; the escaping
	// rules are the same file format and a template only ever holds plain settings.
	b.WriteString("#Minecraft server properties — from a hosuto template\n")
	for k, v := range out {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(strings.NewReplacer("\n", `\n`, "\r", `\r`).Replace(v))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	t, ok := s.visibleTemplate(r.PathValue("tid"), u)
	if !ok {
		writeErr(w, http.StatusNotFound, "No such template")
		return
	}
	if err := s.st.DeleteTemplate(t.ID); err != nil {
		writeErr(w, http.StatusNotFound, "No such template")
		return
	}
	// The record is the truth; a payload left behind by a failed unlink is dead weight, not a bug
	// the user should see reported as a failed delete.
	_ = os.Remove(s.templatePath(t.ID))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── shared: registering the record ────────────────────────────────────────────────────

// reserve performs the half of a create that is identical however the server is being made: validate
// the slug, enforce the quota, clamp the heap, allocate the ports and register the record.
//
// It exists so blank/template/migrate cannot drift on the rules that actually protect the host. The
// record is written BEFORE any files, deliberately: a failure part-way leaves a row the user can see
// and delete, rather than an orphaned directory nobody knows about.
//
// proto carries what the CALLER already knows — name, slug, Minecraft version, loader, heap. It is
// taken as a whole rather than as loose arguments because the store validates the loader on insert:
// a record assembled here and completed by the caller afterwards would be rejected before the caller
// ever got the chance, which is exactly the regression this signature exists to make impossible.
// Everything the HOST owns — owner, ports, rcon password, public host, the default policy — is filled
// in here and never taken from proto.
func (s *Server) reserve(u *auth.User, proto store.Server) (store.Server, int, error) {
	slug := strings.ToLower(strings.TrimSpace(proto.Slug))
	if !store.SlugRe.MatchString(slug) {
		return store.Server{}, http.StatusBadRequest,
			errors.New("The address may use lowercase letters, digits and dashes")
	}
	if s.st.SlugTaken(slug) {
		return store.Server{}, http.StatusConflict, errors.New("That address is already taken")
	}
	if !store.ValidLoader(proto.Loader) {
		return store.Server{}, http.StatusBadRequest, errors.New("Unknown loader")
	}
	max := s.cfg.Int("maxServersPerUser", 3)
	if !u.Can(rights.GroupAdmin) && s.st.CountOwnedBy(u.Username) >= max {
		return store.Server{}, http.StatusForbidden,
			fmt.Errorf("You already have %d servers — remove one first", max)
	}
	port, rconPort, err := s.rt.AllocatePorts()
	if err != nil {
		return store.Server{}, http.StatusServiceUnavailable,
			errors.New("No free port — the server pool is full")
	}
	pass, err := mcfiles.GenRconPassword()
	if err != nil {
		return store.Server{}, http.StatusInternalServerError,
			errors.New("Could not generate a control password")
	}
	name := strings.TrimSpace(proto.Name)
	if name == "" {
		name = slug
	}
	srv := store.Server{
		Slug: slug, Name: name, Owner: u.Username,
		MCVersion: proto.MCVersion, Loader: proto.Loader, LoaderVersion: proto.LoaderVersion,
		HeapMB: s.clampHeap(proto.HeapMB), Port: port, RconPort: rconPort, RconPass: pass,
		Host: s.rt.Host(slug), JoinPolicy: "whitelist",
	}
	srv, err = s.st.CreateServer(srv)
	if err != nil {
		return store.Server{}, http.StatusInternalServerError, errors.New("Could not create the server")
	}
	return srv, 0, nil
}

// clampHeap applies the instance's defaults and ceiling.
func (s *Server) clampHeap(heapMB int) int {
	if heapMB <= 0 {
		heapMB = s.cfg.Int("defaultHeapMB", 2048)
	}
	if lim := s.cfg.Int("maxHeapMB", 4096); heapMB > lim {
		heapMB = lim
	}
	return heapMB
}

// ── create from a template ────────────────────────────────────────────────────────────

// fromTemplate builds a server out of a saved recipe: reserve the record, make the tree, unpack the
// payload, then provision the loader over it. It is the migration path with a trusted archive —
// which is why it shares the unpack and provision steps rather than owning copies of them.
func (s *Server) fromTemplate(ctx context.Context, srv store.Server, t store.Template) (store.Server, error) {
	// The version, loader and heap already came in through reserve's prototype; only the policy is
	// left, because reserve fixes every new server to whitelist by default.
	if t.JoinPolicy != "" {
		srv.JoinPolicy = t.JoinPolicy
	}
	if err := s.rt.Prepare(ctx, srv); err != nil {
		return srv, err
	}
	dir := runtime.Dir(srv.Owner, srv.Slug)
	if _, err := archive.Extract(s.templatePath(t.ID), dir, archive.Limits{}, nil); err != nil {
		return srv, fmt.Errorf("could not unpack the template: %w", err)
	}
	srv, err := s.rt.Provision(ctx, srv, true)
	if err != nil {
		return srv, err
	}
	// The recipe's mod set is restored wholesale: the jars came across in the payload, so the records
	// describe files that are genuinely there. Re-resolving them against Modrinth would be slower and
	// would lose the provenance of anything Modrinth no longer publishes.
	if len(t.Mods) > 0 {
		mods := make([]store.Mod, 0, len(t.Mods))
		for _, m := range t.Mods {
			m.ID = ""
			mods = append(mods, m)
		}
		srv.Mods = mods
	}
	return srv, nil
}

// ── migration ─────────────────────────────────────────────────────────────────────────

// importReq is what the browser sends to migrate a server, in either transport. The JSON body drives
// an FTP pull; a multipart body carries the same fields as form values plus the archive itself.
type importReq struct {
	Name   string `json:"name"`
	Slug   string `json:"slug"`
	Source string `json:"source"` // upload | ftp
	HeapMB int    `json:"heapMB"`

	// The operator may overrule detection. Empty means "work it out from the files", which is the
	// normal case; a value here wins, because they know something the tree does not say.
	MCVersion     string `json:"mcVersion"`
	Loader        string `json:"loader"`
	LoaderVersion string `json:"loaderVersion"`

	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
	Pass string `json:"pass"`
	Path string `json:"path"`
}

// importServer starts a migration and answers with the job to watch.
//
// The upload is spooled to disk INSIDE the request, because the multipart body dies with it — the
// job that runs afterwards has no connection left to read from. Everything after that point is
// background work.
func (s *Server) importServer(w http.ResponseWriter, r *http.Request, u *auth.User) {
	req, zipPath, err := s.readImportReq(r)
	if err != nil {
		if zipPath != "" {
			_ = os.Remove(zipPath)
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Loader != "" && !store.ValidLoader(req.Loader) {
		_ = os.Remove(zipPath)
		writeErr(w, http.StatusBadRequest, "Unknown loader")
		return
	}
	if req.Source == "ftp" && strings.TrimSpace(req.Host) == "" {
		writeErr(w, http.StatusBadRequest, "Enter the server address to copy from")
		return
	}

	// A migration is the one create whose loader is not yet knowable: it is read out of the files,
	// which have not arrived. The record still has to exist first (so a failure leaves something the
	// user can see and delete), so it opens on the operator's override if they gave one and otherwise
	// on vanilla — the only loader that describes a server with nothing installed yet. reconcile()
	// replaces it from what detect found, before anything is installed.
	loader := req.Loader
	if loader == "" {
		loader = "vanilla"
	}
	srv, code, err := s.reserve(u, store.Server{
		Name: req.Name, Slug: req.Slug, HeapMB: req.HeapMB,
		MCVersion: req.MCVersion, Loader: loader, LoaderVersion: req.LoaderVersion,
	})
	if err != nil {
		if zipPath != "" {
			_ = os.Remove(zipPath)
		}
		writeErr(w, code, err.Error())
		return
	}

	job := s.jobs.Start("import", u.Username, "preparing", func(h *jobs.Handle) error {
		if zipPath != "" {
			defer os.Remove(zipPath)
		}
		h.Result(srv.ID)
		return s.runImport(h, srv, req, zipPath)
	})
	writeJSON(w, http.StatusOK, job)
}

// readImportReq accepts either transport and, for an upload, streams the archive to a temp file.
func (s *Server) readImportReq(r *http.Request) (importReq, string, error) {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "multipart/form-data" {
		var req importReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return req, "", errors.New("Malformed request")
		}
		req.Source = "ftp"
		return req, "", nil
	}

	mr, err := r.MultipartReader()
	if err != nil {
		return importReq{}, "", errors.New("Malformed upload")
	}
	if err := os.MkdirAll(s.importDir(), 0o700); err != nil {
		return importReq{}, "", errors.New("Could not stage the upload")
	}
	req := importReq{Source: "upload"}
	var zipPath string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return req, zipPath, errors.New("Malformed upload")
		}
		if p.FileName() == "" {
			// A form field. Bounded because a field is a slug or a version, never a payload.
			b, _ := io.ReadAll(io.LimitReader(p, 4<<10))
			assignField(&req, p.FormName(), strings.TrimSpace(string(b)))
			p.Close()
			continue
		}
		f, err := os.CreateTemp(s.importDir(), ".upload-*.zip")
		if err != nil {
			p.Close()
			return req, zipPath, errors.New("Could not stage the upload")
		}
		zipPath = f.Name()
		n, cerr := io.Copy(f, io.LimitReader(p, maxUpload+1))
		p.Close()
		if err := f.Close(); err == nil && cerr != nil {
			err = cerr
		}
		if cerr != nil {
			return req, zipPath, errors.New("Could not read the upload")
		}
		if n > maxUpload {
			return req, zipPath, fmt.Errorf("The archive is larger than %d GB", maxUpload>>30)
		}
	}
	if zipPath == "" {
		return req, "", errors.New("No file was uploaded")
	}
	return req, zipPath, nil
}

func assignField(req *importReq, name, value string) {
	switch name {
	case "name":
		req.Name = value
	case "slug":
		req.Slug = value
	case "mcVersion":
		req.MCVersion = value
	case "loader":
		req.Loader = value
	case "loaderVersion":
		req.LoaderVersion = value
	case "heapMB":
		req.HeapMB, _ = strconv.Atoi(value)
	}
}

func (s *Server) importDir() string { return filepath.Join(s.dataDir, "import") }

// runImport is the migration itself: files in, then read them, then commit what they say.
//
// A failure here deliberately LEAVES the server record and its files in place. The transfer is the
// expensive part, and most failures at this point ("could not tell which Minecraft version this is")
// are fixable from the Modding tab afterwards — deleting several gigabytes because the last step
// needed a human would be the wrong trade.
func (s *Server) runImport(h *jobs.Handle, srv store.Server, req importReq, zipPath string) error {
	ctx := h.Context()

	h.Phase("preparing")
	if err := s.rt.Prepare(ctx, srv); err != nil {
		return err
	}
	dir := runtime.Dir(srv.Owner, srv.Slug)
	var ev detect.LibEvidence

	// Files land DIRECTLY in the server tree rather than being staged and moved. The tree is setgid
	// with a default ACL, so anything created inside it is readable by the daemon and writable by the
	// run account; a rename from elsewhere would carry its old ownership in and the game could not
	// write its own world.
	if req.Source == "upload" {
		h.Phase("unpacking")
		res, err := archive.Extract(zipPath, dir, archive.Limits{}, h.Add)
		if err != nil {
			return fmt.Errorf("could not unpack the archive: %w", err)
		}
		h.Note(fmt.Sprintf("Unpacked %d files.", res.Files))
		if res.Root != "" {
			h.Note(fmt.Sprintf("The archive wrapped everything in %q; that folder was unwrapped.", res.Root))
		}
		if n := len(res.Skipped); n > 0 {
			h.Note(fmt.Sprintf("%d entries were skipped as unsafe (links, or paths pointing outside the server).", n))
		}
	} else {
		e, err := s.pullFTP(h, req, dir)
		if err != nil {
			return err
		}
		ev = e
	}

	h.Phase("inspecting")
	det, err := detect.Inspect(dir)
	if err != nil {
		return fmt.Errorf("could not read the migrated files: %w", err)
	}
	// What the transfer saw but did not download fills the gaps the local tree cannot answer.
	if det.Loader == "" && ev.Loader != "" {
		det.Loader = ev.Loader
	}
	if det.LoaderVersion == "" && det.Loader == ev.Loader {
		det.LoaderVersion = ev.LoaderVersion
	}
	if det.MCVersion == "" {
		det.MCVersion = ev.MCVersion
	}
	h.Notes(det.Notes)

	// commit persists the fields reconcile and Provision establish (loader, version build, heap, policy)
	// onto the live record as one atomic access. Wholesale replacement would let an import clobber a
	// concurrent write and, on the error paths below, could leave a half-reconciled record visible to
	// another request; a field write touches only what the import owns.
	commit := func(from store.Server) (store.Server, error) {
		return s.st.MutateServer(from.ID, func(cur *store.Server) error {
			cur.Loader = from.Loader
			cur.MCVersion = from.MCVersion
			cur.LoaderVersion = from.LoaderVersion
			cur.JoinPolicy = from.JoinPolicy
			cur.HeapMB = from.HeapMB
			return nil
		})
	}

	srv, err = s.reconcile(ctx, h, srv, det, req)
	if err != nil {
		_, _ = commit(srv) // keep whatever we did establish
		return err
	}

	h.Phase("installing")
	h.Message(loaderName(srv.Loader) + " " + srv.MCVersion)
	// The heap the old host gave it changes the unit's memory limits, so the drop-in is rewritten
	// before the loader goes in. Prepare is re-runnable by design.
	if err := s.rt.Prepare(ctx, srv); err != nil {
		return err
	}
	srv, err = s.rt.Provision(ctx, srv, false)
	if err != nil {
		_, _ = commit(srv)
		return err
	}
	saved, err := commit(srv)
	if err != nil {
		return errors.New("could not save the server")
	}
	srv = saved

	if err := s.adoptMods(ctx, h, &srv, det); err != nil {
		return err
	}
	s.adoptMembers(ctx, h, &srv, det)

	h.Phase("done")
	return nil
}

// reconcile turns what detect read into the server record, with the operator's overrides on top and
// the catalogue as the final check.
//
// The catalogue check is the reason this is a separate step. Detection reads what the old host RAN;
// installing needs something hosuto can actually FETCH, and those differ often enough to matter — a
// Fabric build that has since been pulled, a Paper build number that is not a version at all.
func (s *Server) reconcile(ctx context.Context, h *jobs.Handle, srv store.Server,
	det detect.Result, req importReq) (store.Server, error) {

	srv.Loader = firstNonEmpty(req.Loader, det.Loader)
	srv.MCVersion = firstNonEmpty(req.MCVersion, det.MCVersion)
	srv.LoaderVersion = firstNonEmpty(req.LoaderVersion, det.LoaderVersion)

	if srv.MCVersion == "" {
		return srv, errors.New("could not tell which Minecraft version this server ran — set it in the Modding tab and the files are ready to go")
	}
	if srv.Loader == "" {
		// Vanilla is the honest fallback: it is the only one that runs with no loader present, so a
		// tree that showed no loader evidence probably had none.
		srv.Loader = "vanilla"
		h.Note("No mod loader was found, so this was set up as a vanilla server.")
	}
	if !store.ValidLoader(srv.Loader) {
		return srv, fmt.Errorf("hosuto cannot install %s", srv.Loader)
	}
	if err := s.supported(ctx, srv.Loader, srv.MCVersion); err != nil {
		return srv, err
	}
	// The loader BUILD is not a detail: a modpack is assembled against one, and installing a different
	// one is how a migrated server dies on boot with a registry error that names an innocent mod. So
	// the build the server was running is carried over wherever it can be established, and the two
	// cases where it cannot are both reported rather than quietly resolved to "newest".
	if srv.Loader != "vanilla" && req.LoaderVersion == "" {
		switch {
		case srv.LoaderVersion == "":
			h.Note(fmt.Sprintf("Could not tell which %s build this server ran, so the newest for Minecraft %s was installed — if the server fails to load its mods, set the old build in the Modding tab.",
				loaderName(srv.Loader), srv.MCVersion))
		default:
			known, err := s.vc.LoaderVersions(ctx, srv.Loader, srv.MCVersion)
			if err == nil && !containsStr(known, srv.LoaderVersion) {
				h.Note(fmt.Sprintf("%s %s is no longer published — the newest build for Minecraft %s was installed instead.",
					loaderName(srv.Loader), srv.LoaderVersion, srv.MCVersion))
				srv.LoaderVersion = ""
			} else if err == nil {
				h.Note(fmt.Sprintf("Kept the %s build this server was running (%s).",
					loaderName(srv.Loader), srv.LoaderVersion))
			}
		}
	}

	if det.JoinPolicy != "" {
		srv.JoinPolicy = det.JoinPolicy
	}
	// The heap the old host gave it, unless the operator asked for a specific one.
	if req.HeapMB <= 0 && det.HeapMB > 0 {
		want := s.clampHeap(det.HeapMB)
		if want != det.HeapMB {
			h.Note(fmt.Sprintf("The old host gave this server %d MB; this one allows %d MB.", det.HeapMB, want))
		}
		srv.HeapMB = want
	}
	h.Note(fmt.Sprintf("Recognised %s %s.", loaderName(srv.Loader), srv.MCVersion))
	return srv, nil
}

// ── the FTP pull ──────────────────────────────────────────────────────────────────────

// pullFTP copies a remote tree into the server directory.
//
// The whole listing is walked BEFORE anything is downloaded, for one reason: it is the only way to
// know the total, and a multi-gigabyte transfer with an indeterminate bar is indistinguishable from
// a hung one. Skipped paths are decided here too — see importable.
func (s *Server) pullFTP(h *jobs.Handle, req importReq, dir string) (detect.LibEvidence, error) {
	ctx := h.Context()
	var ev detect.LibEvidence

	h.Phase("connecting")
	h.Message(req.Host)
	c, err := ftp.Dial(ctx, ftp.Config{
		Host: strings.TrimSpace(req.Host), Port: req.Port,
		User: strings.TrimSpace(req.User), Pass: req.Pass,
	})
	if err != nil {
		if errors.Is(err, ftp.ErrAuth) {
			return ev, errors.New("the host refused that username or password")
		}
		return ev, err
	}
	defer c.Close()
	if !c.TLS() {
		h.Note("This host does not offer encrypted FTP, so the transfer was not encrypted.")
	}

	root := strings.TrimSpace(req.Path)
	if root == "" {
		root = "/"
	}

	h.Phase("listing")
	var files []ftp.Entry
	var total int64
	if err := c.Walk(ctx, root, ftp.WalkLimits{}, func(e ftp.Entry) error {
		rel := strings.TrimPrefix(strings.TrimPrefix(e.Path, root), "/")
		// Read the evidence BEFORE the filter. libraries/ is skipped as a download — it is hundreds of
		// megabytes the installer rebuilds — but its paths name the exact loader build this server ran,
		// and that is the one thing the rebuild cannot recover.
		ev.Observe(rel)
		if !importable(rel) {
			return nil
		}
		files = append(files, e)
		total += e.Size
		h.Total(int64(len(files)))
		return nil
	}); err != nil {
		return ev, fmt.Errorf("could not list %s: %w", root, err)
	}
	if len(files) == 0 {
		return ev, fmt.Errorf("found no files under %s on that host", root)
	}

	h.Phase("downloading")
	h.Total(total)
	for _, e := range files {
		if err := ctx.Err(); err != nil {
			return ev, err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(e.Path, root), "/")
		local, ok := safeLocal(dir, rel)
		if !ok {
			continue
		}
		h.Message(rel)
		if _, err := c.Retrieve(ctx, e.Path, local, h.Add); err != nil {
			// One unreadable file must not cost the whole migration: shared hosts routinely deny a
			// lock file or a live log. It is reported and the transfer carries on.
			h.Note(fmt.Sprintf("Could not copy %s — skipped.", rel))
			continue
		}
	}
	h.Note(fmt.Sprintf("Copied %d files from %s.", len(files), req.Host))
	return ev, nil
}

// importable filters the remote tree. Logs, caches and the old host's own launcher wrappers are
// noise that would only slow the transfer; hosuto regenerates its own equivalents.
func importable(rel string) bool {
	if rel == "" {
		return false
	}
	top, _, _ := strings.Cut(rel, "/")
	switch top {
	case "logs", "crash-reports", "cache", ".git", "libraries", "versions", "debug", ".mixin.out":
		return false
	}
	base := path.Base(rel)
	switch base {
	case "session.lock", ".ftpquota", "exec.argv":
		return false
	}
	return true
}

// safeLocal maps a remote relative path to a local one inside dir, refusing anything that would
// escape. The remote host chose these names, so they get the same treatment as an archive's.
func safeLocal(dir, rel string) (string, bool) {
	clean := path.Clean("/" + strings.ReplaceAll(rel, `\`, "/"))
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", false
	}
	abs := filepath.Join(dir, filepath.FromSlash(clean))
	relBack, err := filepath.Rel(dir, abs)
	if err != nil || relBack == ".." || strings.HasPrefix(relBack, ".."+string(filepath.Separator)) {
		return "", false
	}
	return abs, true
}

// ── adopting the mod set ──────────────────────────────────────────────────────────────

// adoptMods turns the jars that came across into hosuto's mod records, so the Modding tab describes
// what is actually installed.
//
// Modrinth's batch hash lookup does the identification: a jar's sha1 IS its identity there, so a mod
// that came from Modrinth is recognised no matter what the previous host named the file. What it
// does not know is recorded as an upload — visible and removable, just without a project behind it.
//
// Only mods/ is adopted. plugins/ is Paper's, and hosuto offers no plugin management at all (the
// Modding tab says so for Paper), so a record there would describe something no operation could act
// on — and uninstalling one would delete the wrong path. The plugins came across and work; they are
// managed in the Files tab, and the note says so.
func (s *Server) adoptMods(ctx context.Context, h *jobs.Handle, srv *store.Server, det detect.Result) error {
	var jars []detect.Jar
	plugins := 0
	for _, j := range det.Mods {
		if strings.HasPrefix(j.Path, "plugins/") {
			plugins++
			continue
		}
		jars = append(jars, j)
	}
	if plugins > 0 {
		h.Note(fmt.Sprintf("%d plugins came across and are in place; hosuto manages plugins in the Files tab.", plugins))
	}
	if len(jars) == 0 {
		return nil
	}

	h.Phase("mods")
	h.Total(int64(len(jars)))

	hashes := make([]string, 0, len(jars))
	for _, j := range jars {
		hashes = append(hashes, j.SHA1)
	}
	// A Modrinth outage must not fail a migration whose files are already in place: without the
	// lookup every jar is simply recorded as an upload.
	byHash, err := s.mr.VersionsByHash(ctx, hashes)
	if err != nil {
		h.Note("Modrinth could not be reached, so the mods were recorded by their files only.")
		byHash = map[string]modrinth.Version{}
	}

	projects := map[string]modrinth.Hit{}
	mods := make([]store.Mod, 0, len(jars))
	matched := 0
	for _, j := range jars {
		h.Add(1)
		h.Message(j.Name)
		ver, ok := byHash[strings.ToLower(j.SHA1)]
		if !ok {
			mods = append(mods, uploadMod(j))
			continue
		}
		hit, seen := projects[ver.ProjectID]
		if !seen {
			if p, err := s.mr.Project(ctx, ver.ProjectID); err == nil {
				hit = p
			}
			projects[ver.ProjectID] = hit
		}
		mods = append(mods, resolvedMod(j, ver, hit))
		matched++
	}
	if err := s.st.SetMods(srv.ID, mods); err != nil {
		return errors.New("could not record the mods")
	}
	srv.Mods = mods
	h.Note(fmt.Sprintf("%d of %d mods were identified on Modrinth; the rest are listed by their file.",
		matched, len(jars)))
	return nil
}

// resolvedMod records a jar Modrinth recognised.
//
// The FILENAME stays the local one and the hashes stay the local file's: the record has to describe
// the file that is actually in mods/, because that is what uninstall deletes and what the client
// export ships. The project, version and environment come from Modrinth — that is the part the tree
// could not tell us, and it is what makes the mod actionable rather than merely listed.
func resolvedMod(j detect.Jar, ver modrinth.Version, hit modrinth.Hit) store.Mod {
	name := hit.Title
	if name == "" {
		name = firstNonEmpty(ver.Name, j.Name)
	}
	m := store.Mod{
		Source: "modrinth", ProjectID: ver.ProjectID, VersionID: ver.ID,
		Name: name, Filename: j.Filename, SHA1: j.SHA1, SHA512: j.SHA512, Size: j.Size,
		ClientSide: orUnknown(hit.ClientSide), ServerSide: orUnknown(hit.ServerSide),
	}
	// The CDN url is only carried when the published file is byte-for-byte the local one; a client
	// export that pointed at a different build than the server runs would desync every player.
	for _, f := range ver.Files {
		if strings.EqualFold(f.SHA1, j.SHA1) {
			m.URL = f.URL
			break
		}
	}
	return m
}

func uploadMod(j detect.Jar) store.Mod {
	return store.Mod{
		Source: "upload", Name: j.Name, Filename: j.Filename,
		SHA1: j.SHA1, SHA512: j.SHA512, Size: j.Size,
		ClientSide: "unknown", ServerSide: "unknown",
	}
}

// ── adopting the members ──────────────────────────────────────────────────────────────

// adoptMembers turns the old server's whitelist into hosuto membership.
//
// This is the one place a migration cannot be complete, and it is honest about why. hosuto's member
// list is made of HOLISTIC USERS — that is the whole point of it, because a grant resolves to a
// Linux account which resolves to a person the landscape knows. The old whitelist is made of
// Minecraft UUIDs. A player is carried across only if somebody on this instance has linked that
// Minecraft account; anybody else has no identity here to attach a grant to, and inventing one would
// be worse than saying so.
//
// The unmatched names are reported rather than dropped in silence, because the owner can act on
// them: invite those people, and their next link puts them straight back on the server.
func (s *Server) adoptMembers(ctx context.Context, h *jobs.Handle, srv *store.Server, det detect.Result) {
	if len(det.Whitelist) == 0 && len(det.Ops) == 0 {
		return
	}
	h.Phase("members")

	// Minecraft UUID → Linux user, over the accounts hosuto already owns.
	byUUID := map[string]string{}
	for user, acc := range s.st.Accounts() {
		byUUID[strings.ToLower(acc.UUID)] = user
	}
	isOp := map[string]bool{}
	for _, o := range det.Ops {
		isOp[strings.ToLower(o.UUID)] = true
	}

	var players, ops []string
	var unmatched []string
	for _, e := range det.Whitelist {
		user, ok := byUUID[strings.ToLower(e.UUID)]
		if !ok {
			unmatched = append(unmatched, e.Name)
			continue
		}
		if user == srv.Owner {
			continue // the owner is always on the list; a grant for them would be noise
		}
		if isOp[strings.ToLower(e.UUID)] {
			ops = append(ops, user)
		} else {
			players = append(players, user)
		}
	}
	// An op who was never on the whitelist still belongs here — on an open server that is the normal
	// shape, and losing the operators would be the worst possible outcome of a migration.
	for _, o := range det.Ops {
		user, ok := byUUID[strings.ToLower(o.UUID)]
		if !ok || user == srv.Owner || containsStr(ops, user) || containsStr(players, user) {
			continue
		}
		ops = append(ops, user)
	}

	for _, g := range []struct {
		members []string
		level   string
	}{{players, "play"}, {ops, "op"}} {
		if len(g.members) == 0 {
			continue
		}
		if _, err := s.st.AddGrant(srv.ID, store.Grant{
			Kind: "adhoc", Level: g.level, Label: "Migrated", Members: g.members,
		}); err != nil {
			h.Note("Could not restore the member list.")
			return
		}
	}
	if fresh, ok := s.st.Server(srv.ID); ok {
		*srv = fresh
	}
	// Rewrites whitelist.json and ops.json from the grants — the single writer of those files, so a
	// migrated server's lists are produced exactly the way every other server's are.
	if err := s.applyMembers(ctx, *srv); err != nil {
		h.Note("Could not write the member list.")
		return
	}
	if n := len(players) + len(ops); n > 0 {
		h.Note(fmt.Sprintf("%d players were matched to accounts here and added to the server.", n))
	}
	if len(unmatched) > 0 {
		h.Note(fmt.Sprintf("%d players on the old whitelist have no linked account here yet (%s) — "+
			"add them as members once they have linked their Minecraft account.",
			len(unmatched), strings.Join(trim(unmatched, 8), ", ")))
	}
}

// ── job status ────────────────────────────────────────────────────────────────────────

// job reports one job's progress. A job is readable by the user who started it (and by an admin);
// there is nothing secret in a progress counter, but a job names a server and a remote host.
func (s *Server) job(w http.ResponseWriter, r *http.Request, u *auth.User) {
	j, ok := s.jobs.Get(r.PathValue("jid"))
	if !ok || !(j.Owner == u.Username || u.Can(rights.GroupAdmin)) {
		writeErr(w, http.StatusNotFound, "No such job")
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// listJobs returns the caller's running and recent jobs, so a browser reload does not lose track of
// a migration that is still going.
func (s *Server) listJobs(w http.ResponseWriter, _ *http.Request, u *auth.User) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.jobs.ByOwner(u.Username)})
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request, u *auth.User) {
	j, ok := s.jobs.Get(r.PathValue("jid"))
	if !ok || !(j.Owner == u.Username || u.Can(rights.GroupAdmin)) {
		writeErr(w, http.StatusNotFound, "No such job")
		return
	}
	if !s.jobs.Cancel(j.ID) {
		writeErr(w, http.StatusConflict, "That job has already finished")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── helpers ───────────────────────────────────────────────────────────────────────────

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func trim(xs []string, n int) []string {
	if len(xs) <= n {
		return xs
	}
	return append(append([]string{}, xs[:n]...), "…")
}
