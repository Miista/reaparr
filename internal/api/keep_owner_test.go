package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Miista/reaparr/internal/auth"
	"github.com/Miista/reaparr/internal/store"
)

// fakeLibrary behaves like Radarr/Sonarr as far as keeping goes: SetKept
// changes what Library reports, and every call is remembered.
type fakeLibrary struct {
	Sweeper // anything not overridden would nil-panic, which is what a test wants
	items   []LibraryItem
	calls   []setKeptCall
	failSet bool
}

type setKeptCall struct {
	refs []LibraryRef
	keep bool
}

func (f *fakeLibrary) Library() ([]LibraryItem, error) {
	out := make([]LibraryItem, len(f.items))
	copy(out, f.items)
	return out, nil
}

func (f *fakeLibrary) Kept(refs []LibraryRef) (map[LibraryRef]bool, error) {
	out := map[LibraryRef]bool{}
	for _, r := range refs {
		for _, it := range f.items {
			if it.Service == r.Service && it.ID == r.ID {
				out[r] = it.Kept // a title not in the library stays out, like the real thing
			}
		}
	}
	return out, nil
}

func (f *fakeLibrary) SetKept(refs []LibraryRef, keep bool) error {
	if f.failSet {
		return http.ErrAbortHandler
	}
	f.calls = append(f.calls, setKeptCall{refs: refs, keep: keep})
	for _, r := range refs {
		for i := range f.items {
			if f.items[i].Service == r.Service && f.items[i].ID == r.ID {
				f.items[i].Kept = keep
			}
		}
	}
	return nil
}

// switchAuth answers every request as whoever *p currently is, so one test
// can be Alice, then Bob, then an admin.
type switchAuth struct{ p *auth.Principal }

func (s switchAuth) Principal(*http.Request) (auth.Principal, bool) { return *s.p, true }

var (
	alice = auth.Principal{Sub: "sub-alice", Name: "Alice", Roles: []auth.Role{auth.User}}
	bob   = auth.Principal{Sub: "sub-bob", Name: "Bob", Roles: []auth.Role{auth.User}}
	admin = auth.Principal{Sub: "sub-root", Name: "Root", Roles: []auth.Role{auth.User, auth.Admin}}
)

type keepEnv struct {
	t    *testing.T
	dir  string
	st   *store.Store
	lib  *fakeLibrary
	mux  *http.ServeMux
	as   *auth.Principal
	logs int
}

func newKeepEnv(t *testing.T, items ...LibraryItem) *keepEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := &keepEnv{t: t, dir: dir, st: st, lib: &fakeLibrary{items: items}, as: &auth.Principal{}}
	srv := &Server{store: st, sweeper: e.lib, auth: switchAuth{p: e.as}, log: zerolog.Nop()}
	e.mux = http.NewServeMux()
	srv.Routes(e.mux)
	return e
}

func (e *keepEnv) as_(p auth.Principal) { *e.as = p }

func movie(id int, kept bool) LibraryItem {
	return LibraryItem{Service: "radarr", ID: id, Title: "Movie", Kind: "movie", Kept: kept}
}

func (e *keepEnv) keep(keep bool, refs ...LibraryRef) (int, map[string]json.RawMessage) {
	e.t.Helper()
	body, _ := json.Marshal(map[string]any{"items": refs, "keep": keep})
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/library/keep", bytes.NewReader(body)))
	var out map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (e *keepEnv) library() map[int]LibraryItem {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/library", nil))
	if rec.Code != http.StatusOK {
		e.t.Fatalf("library: %d", rec.Code)
	}
	var out struct {
		Items []LibraryItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		e.t.Fatal(err)
	}
	m := map[int]LibraryItem{}
	for _, it := range out.Items {
		m[it.ID] = it
	}
	return m
}

func (e *keepEnv) keeper(id int) (store.Keeper, bool) {
	var k store.Keeper
	var ok bool
	e.st.View(func(s store.State) { k, ok = s.Keepers[refKey("radarr", id)] })
	return k, ok
}

func ref(id int) LibraryRef { return LibraryRef{Service: "radarr", ID: id} }

func TestKeepRecordsWhoKept(t *testing.T) {
	e := newKeepEnv(t, movie(1, false))
	e.as_(alice)
	if code, _ := e.keep(true, ref(1)); code != http.StatusOK {
		t.Fatalf("keep: %d", code)
	}
	if k, ok := e.keeper(1); !ok || k.Sub != "sub-alice" || k.Name != "Alice" {
		t.Fatalf("keeper = %+v (recorded %v), want Alice", k, ok)
	}
	if len(e.lib.calls) != 1 || !e.lib.calls[0].keep {
		t.Fatalf("expected one keep call, got %+v", e.lib.calls)
	}
}

func TestKeepingAKeptTitleChangesNothing(t *testing.T) {
	e := newKeepEnv(t, movie(1, false))
	e.as_(alice)
	e.keep(true, ref(1))
	e.as_(bob)
	if code, _ := e.keep(true, ref(1)); code != http.StatusOK {
		t.Fatalf("keep of a kept title: %d", code)
	}
	if k, _ := e.keeper(1); k.Sub != "sub-alice" {
		t.Fatalf("Bob must not take over the title: owner is %q", k.Sub)
	}
	if len(e.lib.calls) != 1 {
		t.Fatalf("no second tag call expected, got %d calls", len(e.lib.calls))
	}
}

func TestOnlyTheKeeperOrAnAdminMayUnkeep(t *testing.T) {
	e := newKeepEnv(t, movie(1, false), movie(2, false))
	e.as_(alice)
	e.keep(true, ref(1), ref(2))

	// Bob may not.
	e.as_(bob)
	code, out := e.keep(false, ref(1))
	if code != http.StatusForbidden {
		t.Fatalf("Bob unkeeping Alice's title: %d, want 403", code)
	}
	var refused []LibraryRef
	_ = json.Unmarshal(out["refused"], &refused)
	if len(refused) != 1 || refused[0].ID != 1 {
		t.Fatalf("refused = %+v, want movie 1", refused)
	}
	if !e.library()[1].Kept {
		t.Fatal("the title must still be kept")
	}
	if _, ok := e.keeper(1); !ok {
		t.Fatal("the record must survive a refused unkeep")
	}

	// Alice may unkeep her own.
	e.as_(alice)
	if code, _ := e.keep(false, ref(1)); code != http.StatusOK {
		t.Fatalf("Alice unkeeping her own: %d", code)
	}
	if e.library()[1].Kept {
		t.Fatal("the title should be unkept")
	}
	if _, ok := e.keeper(1); ok {
		t.Fatal("the record should be gone with the tag")
	}

	// An admin may unkeep anyone's.
	e.as_(admin)
	if code, _ := e.keep(false, ref(2)); code != http.StatusOK {
		t.Fatalf("admin unkeeping Alice's title: %d", code)
	}
	if e.library()[2].Kept {
		t.Fatal("the admin's unkeep should have worked")
	}
}

func TestKeepsWithNoRecordedOwnerAreAnAdminsToUnkeep(t *testing.T) {
	e := newKeepEnv(t, movie(1, true)) // kept before owners were recorded
	e.as_(alice)
	if code, _ := e.keep(false, ref(1)); code != http.StatusForbidden {
		t.Fatalf("a user unkeeping an ownerless keep: %d, want 403", code)
	}
	e.as_(admin)
	if code, _ := e.keep(false, ref(1)); code != http.StatusOK || e.library()[1].Kept {
		t.Fatalf("an admin should be able to: %d", code)
	}
}

func TestAMixedRequestDoesWhatItMayAndSaysWhatItRefused(t *testing.T) {
	e := newKeepEnv(t, movie(1, false), movie(2, false))
	e.as_(alice)
	e.keep(true, ref(1))
	e.as_(bob)
	e.keep(true, ref(2))

	e.as_(alice)
	code, out := e.keep(false, ref(1), ref(2))
	if code != http.StatusOK {
		t.Fatalf("partly allowed should be 200, got %d", code)
	}
	var refused []LibraryRef
	_ = json.Unmarshal(out["refused"], &refused)
	if len(refused) != 1 || refused[0].ID != 2 {
		t.Fatalf("refused = %+v, want only movie 2 (Bob's)", refused)
	}
	lib := e.library()
	if lib[1].Kept || !lib[2].Kept {
		t.Fatalf("Alice's should be unkept and Bob's still kept: %+v %+v", lib[1], lib[2])
	}
}

func TestUnkeepingSomethingNotKeptDropsAStaleRecord(t *testing.T) {
	e := newKeepEnv(t, movie(1, false))
	e.as_(alice)
	e.keep(true, ref(1))
	e.lib.items[0].Kept = false // the tag was removed by hand in Radarr
	if code, _ := e.keep(false, ref(1)); code != http.StatusOK {
		t.Fatalf("unkeep: %d", code)
	}
	if _, ok := e.keeper(1); ok {
		t.Fatal("a record for a title that is no longer kept should go")
	}
	// And a new keep, by someone else, simply becomes the owner.
	e.as_(bob)
	e.keep(true, ref(1))
	if k, _ := e.keeper(1); k.Sub != "sub-bob" {
		t.Fatalf("owner = %q, want Bob", k.Sub)
	}
}

func TestLibraryTellsEachPersonWhatTheyMayDo(t *testing.T) {
	e := newKeepEnv(t, movie(1, false), movie(2, false), movie(3, true))
	e.as_(alice)
	e.keep(true, ref(1))
	e.as_(bob)
	e.keep(true, ref(2))

	// Alice: can unkeep her own only; no names; the ownerless keep is not hers.
	e.as_(alice)
	lib := e.library()
	if !lib[1].CanUnkeep || lib[2].CanUnkeep || lib[3].CanUnkeep {
		t.Fatalf("Alice's can_unkeep: %v %v %v, want true false false", lib[1].CanUnkeep, lib[2].CanUnkeep, lib[3].CanUnkeep)
	}
	for id, it := range lib {
		if it.KeptBy != "" {
			t.Fatalf("a user must not see who kept (movie %d: %q)", id, it.KeptBy)
		}
	}

	// An admin: can unkeep every kept title and sees who kept each.
	e.as_(admin)
	lib = e.library()
	if !lib[1].CanUnkeep || !lib[2].CanUnkeep || !lib[3].CanUnkeep {
		t.Fatal("an admin may unkeep every kept title")
	}
	if lib[1].KeptBy != "Alice" || lib[2].KeptBy != "Bob" || lib[3].KeptBy != "" {
		t.Fatalf("kept_by = %q %q %q, want Alice, Bob and nothing for the ownerless keep", lib[1].KeptBy, lib[2].KeptBy, lib[3].KeptBy)
	}
}

func TestKeepRejectsAnUnknownServiceAndRecordsNothingOnFailure(t *testing.T) {
	e := newKeepEnv(t, movie(1, false))
	e.as_(alice)
	if code, _ := e.keep(true, LibraryRef{Service: "lidarr", ID: 1}); code != http.StatusBadRequest {
		t.Fatalf("unknown service: %d, want 400", code)
	}
	e.lib.failSet = true
	if code, _ := e.keep(true, ref(1)); code != http.StatusInternalServerError {
		t.Fatalf("a failing Radarr: %d, want 500", code)
	}
	if _, ok := e.keeper(1); ok {
		t.Fatal("nothing may be recorded when the tag was not set")
	}
}

func TestKeeperRecordsSurviveARestart(t *testing.T) {
	e := newKeepEnv(t, movie(1, false))
	e.as_(alice)
	e.keep(true, ref(1))
	st, err := store.Open(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	var k store.Keeper
	st.View(func(s store.State) { k = s.Keepers["radarr:1"] })
	if k.Sub != "sub-alice" {
		t.Fatalf("after reopening the store, keeper = %+v", k)
	}
}

func TestMayUnkeep(t *testing.T) {
	owned := store.Keeper{Sub: "sub-alice"}
	tests := []struct {
		name  string
		who   auth.Principal
		owner store.Keeper
		has   bool
		want  bool
	}{
		{"the keeper", alice, owned, true, true},
		{"someone else", bob, owned, true, false},
		{"an admin", admin, owned, true, true},
		{"an admin, no owner", admin, store.Keeper{}, false, true},
		{"a user, no owner", alice, store.Keeper{}, false, false},
		{"empty sub never matches an empty owner", auth.Principal{Roles: []auth.Role{auth.User}}, store.Keeper{Sub: ""}, true, false},
	}
	for _, tc := range tests {
		if got := mayUnkeep(tc.who, tc.owner, tc.has); got != tc.want {
			t.Errorf("%s: mayUnkeep = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// breakStore makes every later save of the store fail: the temp file it
// writes first is a directory.
func (e *keepEnv) breakStore() {
	e.t.Helper()
	if err := os.Mkdir(filepath.Join(e.dir, "config.json.tmp"), 0o700); err != nil {
		e.t.Fatal(err)
	}
}

func TestKeepWhoseOwnerCannotBeRecordedIsNotKept(t *testing.T) {
	e := newKeepEnv(t, movie(1, false))
	e.as_(alice)
	e.breakStore()
	if code, _ := e.keep(true, ref(1)); code != http.StatusInternalServerError {
		t.Fatalf("keep with a failing store: %d, want 500", code)
	}
	if len(e.lib.calls) != 0 || e.lib.items[0].Kept {
		t.Fatal("the tag must not be set without an owner to record: the keeper would be locked out of unkeeping")
	}
}

func TestAFailedTagUndoesTheRecordedOwner(t *testing.T) {
	e := newKeepEnv(t, movie(1, false))
	e.as_(alice)
	e.lib.failSet = true
	e.keep(true, ref(1))
	if _, ok := e.keeper(1); ok {
		t.Fatal("an owner was recorded for a title that did not get the tag")
	}
}

func TestUnkeepSucceedsEvenIfForgettingTheOwnerFails(t *testing.T) {
	e := newKeepEnv(t, movie(1, false))
	e.as_(alice)
	e.keep(true, ref(1))
	e.breakStore()
	if code, _ := e.keep(false, ref(1)); code != http.StatusOK {
		t.Fatalf("unkeep: %d, want 200: the tag is gone, which is what was asked", code)
	}
	if e.lib.items[0].Kept {
		t.Fatal("the title is still kept")
	}
}

func TestTitlesNotInTheLibraryAreIgnored(t *testing.T) {
	e := newKeepEnv(t, movie(1, false))
	e.as_(alice)

	if code, _ := e.keep(true, ref(99)); code != http.StatusNotFound {
		t.Fatalf("keeping a title that does not exist: %d, want 404", code)
	}
	if _, ok := e.keeper(99); ok || len(e.lib.calls) != 0 {
		t.Fatal("nothing may be tagged or recorded for a title that does not exist")
	}

	// A record for a title that has vanished is not ours to judge by its
	// absence: it stays until the title can be seen again.
	if err := e.st.Update(func(s *store.State) {
		s.Keepers = map[string]store.Keeper{"radarr:99": {Sub: "sub-alice", Name: "Alice"}}
	}); err != nil {
		t.Fatal(err)
	}
	e.keep(false, ref(99))
	if _, ok := e.keeper(99); !ok {
		t.Fatal("an unkeep of an unknown title must not delete its record")
	}

	// Mixed with a real title, the unknown one is skipped and the rest proceeds.
	if code, _ := e.keep(true, ref(99), ref(1)); code != http.StatusOK {
		t.Fatalf("mixed request: %d, want 200", code)
	}
	if !e.lib.items[0].Kept {
		t.Fatal("the title that exists should have been kept")
	}
	if len(e.lib.calls) != 1 || len(e.lib.calls[0].refs) != 1 || e.lib.calls[0].refs[0] != ref(1) {
		t.Fatalf("only the real title may be tagged, got %+v", e.lib.calls)
	}
}
