package store

import (
	"os"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newServer(t *testing.T, s *Store, mods ...Mod) Server {
	t.Helper()
	srv, err := s.CreateServer(Server{Slug: "test-server", Name: "Test", Owner: "ada", Loader: "fabric", JoinPolicy: "whitelist", Mods: mods})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// Every operation the UI offers addresses a mod by id, so a stored mod without one would be listed
// and then impossible to remove. A migration and a template both hand over mod sets that have never
// been through AddMod, which is why the store fills the id in rather than trusting the caller.
func TestModsAlwaysGetAnIdentity(t *testing.T) {
	s := open(t)
	srv := newServer(t, s)

	// SetMods — the migration and template path.
	if err := s.SetMods(srv.ID, []Mod{
		{Source: "modrinth", Name: "Sodium", Filename: "sodium.jar"},
		{Source: "upload", Name: "Mystery", Filename: "mystery.jar"},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Server(srv.ID)
	assertIdentified(t, got.Mods, 2)

	// The ids must be distinct, or removing one would remove the wrong mod.
	if got.Mods[0].ID == got.Mods[1].ID {
		t.Fatalf("two mods share the id %q", got.Mods[0].ID)
	}

	// An existing id must survive a rewrite — setVersion re-resolves mods in place and the UI is
	// still holding those ids.
	got.Mods[0].Name = "Sodium (renamed)"
	keep := got.Mods[0].ID
	if err := s.SetMods(srv.ID, got.Mods); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Server(srv.ID)
	if after.Mods[0].ID != keep {
		t.Fatalf("id changed on rewrite: %q → %q", keep, after.Mods[0].ID)
	}
}

// A template restores its recipe's mod set through CreateServer/UpdateServer rather than SetMods, so
// those paths must hold the same invariant.
func TestCreateAndUpdateIdentifyMods(t *testing.T) {
	s := open(t)
	srv := newServer(t, s, Mod{Source: "modrinth", Name: "Sodium", Filename: "sodium.jar"})
	assertIdentified(t, srv.Mods, 1)

	srv.Mods = append(srv.Mods, Mod{Source: "upload", Name: "Late", Filename: "late.jar"})
	if err := s.UpdateServer(srv); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Server(srv.ID)
	assertIdentified(t, got.Mods, 2)
}

func TestRemoveMigratedModWorks(t *testing.T) {
	s := open(t)
	srv := newServer(t, s)
	if err := s.SetMods(srv.ID, []Mod{{Source: "upload", Name: "Mystery", Filename: "mystery.jar"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Server(srv.ID)
	removed, err := s.RemoveMod(srv.ID, got.Mods[0].ID)
	if err != nil {
		t.Fatalf("a migrated mod could not be removed: %v", err)
	}
	if removed.Filename != "mystery.jar" {
		t.Fatalf("removed %+v", removed)
	}
}

func assertIdentified(t *testing.T, mods []Mod, want int) {
	t.Helper()
	if len(mods) != want {
		t.Fatalf("got %d mods, want %d", len(mods), want)
	}
	for _, m := range mods {
		if m.ID == "" {
			t.Fatalf("mod %q has no id", m.Filename)
		}
		if m.Added == 0 {
			t.Fatalf("mod %q has no timestamp", m.Filename)
		}
	}
}

// ── templates ─────────────────────────────────────────────────────────────────────────

func TestTemplateRoundTrip(t *testing.T) {
	s := open(t)
	tpl, err := s.CreateTemplate(Template{
		Name: "Modpack", Owner: "ada", MCVersion: "1.21.1", Loader: "fabric",
		HeapMB: 4096, JoinPolicy: "whitelist", IncludeWorld: true, Size: 1234,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tpl.ID == "" || tpl.Created == 0 || tpl.Game != "minecraft" {
		t.Fatalf("template not stamped: %+v", tpl)
	}
	if got, ok := s.Template(tpl.ID); !ok || got.Name != "Modpack" || !got.IncludeWorld {
		t.Fatalf("readback = %+v ok=%v", got, ok)
	}
	if err := s.DeleteTemplate(tpl.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Template(tpl.ID); ok {
		t.Fatal("template survived deletion")
	}
	if err := s.DeleteTemplate(tpl.ID); err != ErrNotFound {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

func TestTemplateRejectsIncomplete(t *testing.T) {
	s := open(t)
	for _, tpl := range []Template{
		{Owner: "ada", Loader: "fabric"},             // no name
		{Name: "x", Loader: "fabric"},                // no owner
		{Name: "x", Owner: "ada", Loader: "forge"},   // a loader hosuto cannot install
		{Name: "  ", Owner: "ada", Loader: "fabric"}, // whitespace is not a name
	} {
		if _, err := s.CreateTemplate(tpl); err != ErrInvalid {
			t.Fatalf("CreateTemplate(%+v) = %v, want ErrInvalid", tpl, err)
		}
	}
}

// Templates must survive a reload, and a state file written before templates existed must still
// open — the map is absent there, and a nil map would panic on the first save.
func TestTemplatesPersistAndOldStateStillOpens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTemplate(Template{Name: "Keep", Owner: "ada", Loader: "paper"}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.Templates()) != 1 {
		t.Fatalf("templates did not survive a reload: %+v", reopened.Templates())
	}

	// A pre-templates state file.
	legacy := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(legacy, []byte(`{"accounts":{},"servers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := Open(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.CreateTemplate(Template{Name: "New", Owner: "ada", Loader: "fabric"}); err != nil {
		t.Fatalf("a legacy state file could not take a template: %v", err)
	}
}
