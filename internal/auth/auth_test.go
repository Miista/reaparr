package auth

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/miista/authy"
)

func withUser(u authy.User, ok bool) func(*http.Request) (authy.User, bool) {
	return func(*http.Request) (authy.User, bool) { return u, ok }
}

func TestRoles(t *testing.T) {
	all := Config{UserGroup: "media-users", AdminGroup: "media-admins", GateOn: true}
	tests := []struct {
		name   string
		cfg    Config
		user   authy.User
		signed bool
		want   []Role
		wantOK bool
	}{
		{"gate off: everyone holds every role", Config{GateOn: false}, authy.User{}, false, All, true},
		{"not signed in", all, authy.User{}, false, nil, false},
		{"signed in with an empty subject", all, authy.User{Subject: ""}, true, nil, false},
		{"local account is the owner", all, authy.User{Subject: "local:owner"}, true, All, true},
		{"no roles configured: anyone signed in holds every role", Config{GateOn: true}, authy.User{Subject: "a"}, true, All, true},
		{"admin group alone gives only admin", all, authy.User{Subject: "a", Groups: []string{"media-admins"}}, true, []Role{Admin}, true},
		{"user group", all, authy.User{Subject: "a", Groups: []string{"media-users"}}, true, []Role{User}, true},
		{"every group gives every role", all, authy.User{Subject: "a", Groups: []string{"media-users", "media-admins"}}, true, All, true},
		{"in no group: no roles", all, authy.User{Subject: "a", Groups: []string{"users"}}, true, nil, true},
		{"no groups claim at all: no roles", all, authy.User{Subject: "a"}, true, nil, true},
		{"group match is exact", all, authy.User{Subject: "a", Groups: []string{"Media-Admins"}}, true, nil, true},
		{"user group empty: anyone signed in is a user", Config{AdminGroup: "media-admins", GateOn: true}, authy.User{Subject: "a", Groups: []string{"whatever"}}, true, []Role{User}, true},
		{"user group empty, admin still from its group", Config{AdminGroup: "media-admins", GateOn: true}, authy.User{Subject: "a", Groups: []string{"media-admins"}}, true, []Role{User, Admin}, true},
		{"a former curator group means nothing now", all, authy.User{Subject: "a", Groups: []string{"media-curators", "media-users"}}, true, []Role{User}, true},
		{"admin group unset gives admin to nobody", Config{UserGroup: "media-users", GateOn: true}, authy.User{Subject: "a", Groups: []string{"media-admins", "media-users"}}, true, []Role{User}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := New(tc.cfg)
			a.from = withUser(tc.user, tc.signed)
			p, ok := a.Principal(httptest.NewRequest(http.MethodGet, "/", nil))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && !reflect.DeepEqual(p.Roles, tc.want) {
				t.Fatalf("roles = %v, want %v", p.Roles, tc.want)
			}
		})
	}
}

func TestHas(t *testing.T) {
	p := Principal{Roles: []Role{User}}
	if !p.Has(User) || p.Has(Admin) {
		t.Fatal("Has must report exactly the roles held")
	}
	if !p.HasAny(Admin, User) || p.HasAny(Admin) || p.HasAny() {
		t.Fatal("HasAny must be true only when some listed role is held")
	}
	if (Principal{}).HasAny(User, Admin) {
		t.Fatal("no roles must match nothing")
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("REAPARR_ROLE_USER_GROUP", " media-users ")
	t.Setenv("REAPARR_ROLE_ADMIN_GROUP", "")
	c := ConfigFromEnv(true)
	if c.UserGroup != "media-users" || c.AdminGroup != "" || !c.GateOn {
		t.Fatalf("unexpected config: %+v", c)
	}
	if !c.Configured() {
		t.Fatal("roles should count as configured with any group set")
	}
	if (Config{}).Configured() {
		t.Fatal("no groups set should not count as configured")
	}
}
