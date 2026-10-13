// Package auth decides what a signed-in person may do. The login itself (the
// session, the identity provider, the local account) is authy's; this turns
// who it says is signed in into one of four roles:
//
//	none     signed in, but not let in: sees a "no access" page and nothing else
//	user     may see what is due and the library, and keep or unkeep titles
//	curator  may also delete due titles by hand
//	admin    everything, including settings and connections
//
// Roles come from groups at the identity provider, so who is who stays
// there. With no group configured at all, everyone the provider lets in is
// an admin, which is how reaparr behaved before roles existed.
package auth

import (
	"net/http"
	"os"
	"strings"

	"github.com/miista/authy"
)

// Role is what someone may do. A higher role may do everything a lower one may.
type Role int

const (
	None Role = iota
	User
	Curator
	Admin
)

func (r Role) String() string {
	switch r {
	case User:
		return "user"
	case Curator:
		return "curator"
	case Admin:
		return "admin"
	}
	return "none"
}

// AtLeast reports whether r may do what min may.
func (r Role) AtLeast(min Role) bool { return r >= min }

// Principal is who a request is from and what they may do.
type Principal struct {
	Sub   string
	Name  string
	Email string
	Role  Role
	Local bool // signed in with the local account rather than the provider
}

// Config says how groups map to roles. A group left empty grants its role to
// nobody, except UserGroup: empty means anyone the provider lets in is at
// least a user. With all three empty, everyone is an admin.
type Config struct {
	UserGroup    string
	CuratorGroup string
	AdminGroup   string
	// GateOn is false when there is no login at all (REAPARR_AUTH_DISABLED):
	// the app is then behind something else, and everyone is an admin.
	GateOn bool
}

// ConfigFromEnv reads REAPARR_ROLE_USER_GROUP, REAPARR_ROLE_CURATOR_GROUP
// and REAPARR_ROLE_ADMIN_GROUP.
func ConfigFromEnv(gateOn bool) Config {
	return Config{
		UserGroup:    strings.TrimSpace(os.Getenv("REAPARR_ROLE_USER_GROUP")),
		CuratorGroup: strings.TrimSpace(os.Getenv("REAPARR_ROLE_CURATOR_GROUP")),
		AdminGroup:   strings.TrimSpace(os.Getenv("REAPARR_ROLE_ADMIN_GROUP")),
		GateOn:       gateOn,
	}
}

// Configured reports whether any group is set, i.e. whether roles are in use.
func (c Config) Configured() bool {
	return c.UserGroup != "" || c.CuratorGroup != "" || c.AdminGroup != ""
}

// Auth resolves a request to a Principal.
type Auth struct {
	cfg  Config
	from func(*http.Request) (authy.User, bool) // authy.UserFrom; replaced in tests
}

// New returns an Auth.
func New(cfg Config) *Auth {
	return &Auth{cfg: cfg, from: authy.UserFrom}
}

// Principal reports who the request is from. It is false when nobody is
// signed in: with a login in place, "no identity" must never read as "anyone".
func (a *Auth) Principal(r *http.Request) (Principal, bool) {
	if !a.cfg.GateOn {
		return Principal{Sub: "anonymous", Name: "anonymous", Role: Admin, Local: true}, true
	}
	u, ok := a.from(r)
	if !ok || u.Subject == "" {
		return Principal{}, false
	}
	return Principal{Sub: u.Subject, Name: u.Name, Email: u.Email, Local: isLocal(u), Role: a.roleFor(u)}, true
}

// isLocal reports whether the person signed in with the local account. authy
// reserves the "local:" subject namespace for it.
func isLocal(u authy.User) bool { return strings.HasPrefix(u.Subject, "local:") }

// roleFor is the role the identity provider's data gives. The highest
// matching group wins.
func (a *Auth) roleFor(u authy.User) Role {
	switch {
	case isLocal(u):
		return Admin // the account made on first launch is the owner
	case !a.cfg.Configured():
		return Admin // no roles in use: as before roles existed
	case a.cfg.AdminGroup != "" && u.InGroup(a.cfg.AdminGroup):
		return Admin
	case a.cfg.CuratorGroup != "" && u.InGroup(a.cfg.CuratorGroup):
		return Curator
	case a.cfg.UserGroup == "" || u.InGroup(a.cfg.UserGroup):
		return User
	}
	return None
}
