package database

import (
	"fmt"
	"net/url"
	"strings"
)

// The checks every entry point runs right after loading its configuration, before it
// opens anything (contracts/configuration-and-startup.md §1–§2), so a refusal never
// leaves a file or directory behind.

const acceptedSchemes = "postgres, postgresql, sqlite"

// ParseURL reports which backend a database URL selects. postgres and postgresql
// select PostgreSQL and are passed on as they are; sqlite selects SQLite and must name
// an absolute path, with no host, query or fragment. Anything else — another scheme, no
// scheme (a keyword/value connection string), an empty URL — is refused with a message
// naming what is accepted. A refused URL is shown with its password replaced.
func ParseURL(raw string) (Backend, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("database.url is empty; accepted schemes: %s", acceptedSchemes)
	}
	u, err := url.Parse(raw)
	if err != nil {
		if i := strings.Index(raw, "://"); i > 0 {
			// A scheme is present but the URL still will not parse — almost always an
			// unescaped % in the password. err is not echoed: it quotes the raw URL verbatim.
			return "", fmt.Errorf("database.url is not a valid URL; percent-encode reserved characters in the user name and password")
		}
		return "", fmt.Errorf("database.url has no scheme; accepted schemes: %s", acceptedSchemes)
	}
	if u.Scheme == "" {
		return "", fmt.Errorf("database.url has no scheme; accepted schemes: %s", acceptedSchemes)
	}
	switch strings.ToLower(u.Scheme) {
	case "postgres", "postgresql":
		return Postgres, nil
	case "sqlite":
		if u.Opaque != "" || u.User != nil || u.Host != "" || !strings.HasPrefix(u.Path, "/") ||
			u.Path == "/" || strings.HasSuffix(u.Path, "/") ||
			u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return "", fmt.Errorf("database.url %q is not a valid SQLite URL; expected sqlite:///absolute/path/lightstack.db (absolute path, no host, no query parameters)", RedactURL(raw))
		}
		return SQLite, nil
	}
	return "", fmt.Errorf("database.url scheme %q is not supported; accepted schemes: %s", u.Scheme, acceptedSchemes)
}

// CheckDeclaredBackend checks the backend LIGHTSTACK_DB_BACKEND declares, if any,
// against the one the URL selects — the guard for a deployment that means to run on
// one backend and is configured for the other.
func CheckDeclaredBackend(declared string, selected Backend) error {
	switch b := Backend(declared); b {
	case "":
		return nil
	case Postgres, SQLite:
		if b != selected {
			return fmt.Errorf("LIGHTSTACK_DB_BACKEND=%s but database.url selects %s", declared, selected)
		}
		return nil
	}
	return fmt.Errorf("LIGHTSTACK_DB_BACKEND=%q is not valid; accepted values: postgres, sqlite", declared)
}

// RedactURL returns rawURL for messages and logs: as written, unless it carries a
// password, which is replaced. A password travels in the userinfo or in a password= query
// parameter (pgx honours both). A string that does not parse as a URL is not echoed at all.
func RedactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" {
		return "<unparsable database URL>"
	}
	masked := false
	if q := u.Query(); q.Has("password") {
		q.Set("password", "xxxxx")
		u.RawQuery = q.Encode()
		masked = true
	}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
			masked = true
		}
	}
	if !masked {
		return rawURL // nothing to replace: keep the original formatting
	}
	return u.String()
}
