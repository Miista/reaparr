// Package auth says which roles a signed-in person holds. The login itself
// (the session, the identity provider, the local account) is authy's; this
// turns the groups it reports into roles:
//
//	user   may see what is due and the library, and keep titles (and unkeep
//	       the ones they kept themselves)
//	admin  may do everything else: delete due titles by hand, unkeep anyone's,
//	       see who kept what, and change settings and connections
//
// A person holds every role whose group they are in, and nothing else. Each
// route then lists the roles it accepts (see internal/api), so the roles are
// not ordered: what an admin may do is whatever lists "admin". With no group
// configured at all, everyone the provider lets in holds every role, which
// is how reaparr behaved before roles existed. With OIDC on, reaparr refuses
// to start in that state unless REAPARR_ROLES_DISABLED=true (see Check).
package auth

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/miista/authy"
)

// Role is a name an action may require.
type Role string

const (
	User  Role = "user"
	Admin Role = "admin"
)

// All is every role, in the order they are reported.
var All = []Role{User, Admin}

// Principal is who a request is from and the roles they hold.
type Principal struct {
	Sub   string
	Name  string
	Email string
	Roles []Role
	Local bool // signed in with the local account rather than the provider
}

// Has reports whether the person holds the role.
func (p Principal) Has(r Role) bool {
	for _, have := range p.Roles {
		if have == r {
			return true
		}
	}
	return false
}

// HasAny reports whether the person holds at least one of the roles.
func (p Principal) HasAny(roles ...Role) bool {
	for _, r := range roles {
		if p.Has(r) {
			return true
		}
	}
	return false
}

// Config names the group behind each role. A group left empty gives its role
// to nobody, except UserGroup: empty means anyone the provider lets in is a
// user. With both empty, everyone holds every role.
type Config struct {
	UserGroup  string
	AdminGroup string
	// GateOn is false when there is no login at all (REAPARR_AUTH_DISABLED):
	// the app is then behind something else, and everyone holds every role.
	GateOn bool
}

// ConfigFromEnv reads REAPARR_ROLE_USER_GROUP and REAPARR_ROLE_ADMIN_GROUP.
func ConfigFromEnv(gateOn bool) Config {
	return Config{
		UserGroup:  strings.TrimSpace(os.Getenv("REAPARR_ROLE_USER_GROUP")),
		AdminGroup: strings.TrimSpace(os.Getenv("REAPARR_ROLE_ADMIN_GROUP")),
		GateOn:     gateOn,
	}
}

// Configured reports whether any group is set, i.e. whether roles are in use.
func (c Config) Configured() bool {
	return c.UserGroup != "" || c.AdminGroup != ""
}

// Check refuses a setup where OIDC lets people in but no role group says who
// may do what: everyone the provider admits would be an admin. Setting
// REAPARR_ROLES_DISABLED=true accepts that on purpose. Without OIDC (local
// account only, or no login) there are no groups to read, so nothing to check.
func (c Config) Check(oidc bool) error {
	if !oidc || c.Configured() || strings.EqualFold(strings.TrimSpace(os.Getenv("REAPARR_ROLES_DISABLED")), "true") {
		return nil
	}
	return errors.New("OIDC is on but neither REAPARR_ROLE_USER_GROUP nor REAPARR_ROLE_ADMIN_GROUP is set, " +
		"so everyone the provider admits would be an admin; set a group, or REAPARR_ROLES_DISABLED=true to allow that")
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
		return Principal{Sub: "anonymous", Name: "anonymous", Roles: All, Local: true}, true
	}
	u, ok := a.from(r)
	if !ok || u.Subject == "" {
		return Principal{}, false
	}
	return Principal{Sub: u.Subject, Name: u.Name, Email: u.Email, Local: isLocal(u), Roles: a.rolesFor(u)}, true
}

// isLocal reports whether the person signed in with the local account. authy
// reserves the "local:" subject namespace for it.
func isLocal(u authy.User) bool { return strings.HasPrefix(u.Subject, "local:") }

// rolesFor is every role the identity provider's groups give.
func (a *Auth) rolesFor(u authy.User) []Role {
	if isLocal(u) || !a.cfg.Configured() {
		return All // the owner's account, or roles not in use: as before roles existed
	}
	var roles []Role
	if a.cfg.UserGroup == "" || u.InGroup(a.cfg.UserGroup) {
		roles = append(roles, User)
	}
	if a.cfg.AdminGroup != "" && u.InGroup(a.cfg.AdminGroup) {
		roles = append(roles, Admin)
	}
	return roles
}
