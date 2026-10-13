package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/miista/authy"
)

func withUser(u authy.User, ok bool) func(*http.Request) (authy.User, bool) {
	return func(*http.Request) (authy.User, bool) { return u, ok }
}

func TestRoleLadder(t *testing.T) {
	all := Config{UserGroup: "media-users", CuratorGroup: "media-curators", AdminGroup: "media-admins", GateOn: true}
	tests := []struct {
		name   string
		cfg    Config
		user   authy.User
		signed bool
		want   Role
		wantOK bool
	}{
		{"gate off: everyone is an admin", Config{GateOn: false}, authy.User{}, false, Admin, true},
		{"not signed in", all, authy.User{}, false, None, false},
		{"signed in with an empty subject", all, authy.User{Subject: ""}, true, None, false},
		{"local account is the owner", all, authy.User{Subject: "local:owner"}, true, Admin, true},
		{"no roles configured: anyone signed in is an admin", Config{GateOn: true}, authy.User{Subject: "a"}, true, Admin, true},
		{"admin group", all, authy.User{Subject: "a", Groups: []string{"media-admins"}}, true, Admin, true},
		{"curator group", all, authy.User{Subject: "a", Groups: []string{"media-curators"}}, true, Curator, true},
		{"user group", all, authy.User{Subject: "a", Groups: []string{"media-users"}}, true, User, true},
		{"highest group wins", all, authy.User{Subject: "a", Groups: []string{"media-users", "media-curators", "media-admins"}}, true, Admin, true},
		{"curator beats user", all, authy.User{Subject: "a", Groups: []string{"media-users", "media-curators"}}, true, Curator, true},
		{"in no group", all, authy.User{Subject: "a", Groups: []string{"users"}}, true, None, true},
		{"no groups claim at all", all, authy.User{Subject: "a"}, true, None, true},
		{"group match is exact", all, authy.User{Subject: "a", Groups: []string{"Media-Admins"}}, true, None, true},
		{"user group empty: anyone signed in is a user", Config{AdminGroup: "media-admins", GateOn: true}, authy.User{Subject: "a", Groups: []string{"whatever"}}, true, User, true},
		{"user group empty, admin still from its group", Config{AdminGroup: "media-admins", GateOn: true}, authy.User{Subject: "a", Groups: []string{"media-admins"}}, true, Admin, true},
		{"curator group unset grants curator to nobody", Config{UserGroup: "media-users", AdminGroup: "media-admins", GateOn: true}, authy.User{Subject: "a", Groups: []string{"media-curators", "media-users"}}, true, User, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := New(tc.cfg)
			a.from = withUser(tc.user, tc.signed)
			p, ok := a.Principal(httptest.NewRequest(http.MethodGet, "/", nil))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && p.Role != tc.want {
				t.Fatalf("role = %s, want %s", p.Role, tc.want)
			}
		})
	}
}

func TestAtLeast(t *testing.T) {
	if !Admin.AtLeast(Curator) || !Curator.AtLeast(User) || !User.AtLeast(User) {
		t.Fatal("a higher role must be at least a lower one")
	}
	if User.AtLeast(Curator) || Curator.AtLeast(Admin) || None.AtLeast(User) {
		t.Fatal("a lower role must not be at least a higher one")
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("REAPARR_ROLE_USER_GROUP", " media-users ")
	t.Setenv("REAPARR_ROLE_CURATOR_GROUP", "media-curators")
	t.Setenv("REAPARR_ROLE_ADMIN_GROUP", "")
	c := ConfigFromEnv(true)
	if c.UserGroup != "media-users" || c.CuratorGroup != "media-curators" || c.AdminGroup != "" || !c.GateOn {
		t.Fatalf("unexpected config: %+v", c)
	}
	if !c.Configured() {
		t.Fatal("roles should count as configured with any group set")
	}
	if (Config{}).Configured() {
		t.Fatal("no groups set should not count as configured")
	}
}
