package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Job home modes for --job-home.
const (
	HomeSpareci = "spareci" // a spareci-owned home per repo (default)
	HomeUser    = "user"    // the user's own home and environment, as before
)

// ResolveJobHome turns a --job-home value into the directory jobs get as HOME.
// "spareci" is a persistent home per repo under root, "user" returns "" (jobs
// keep the user's HOME and environment), and anything else must be an
// absolute path to use instead.
func ResolveJobHome(mode, root, repo string) (string, error) {
	switch mode {
	case "", HomeSpareci:
		return filepath.Join(root, "home", filepath.FromSlash(repo)), nil
	case HomeUser:
		return "", nil
	}
	if !filepath.IsAbs(mode) {
		return "", fmt.Errorf("--job-home must be %q, %q or an absolute path, not %q", HomeSpareci, HomeUser, mode)
	}
	return filepath.Clean(mode), nil
}

// passEnv is what a job inherits from spareci's own environment when it has a
// separate home. Everything else (tool settings like GOPATH or CARGO_HOME,
// tokens like GH_TOKEN, SSH_AUTH_SOCK) is dropped, much as a hosted runner
// starts without the user's shell setup.
var passEnv = map[string]bool{
	"PATH": true, "LANG": true, "LANGUAGE": true, "TZ": true, "TMPDIR": true,
	"USER": true, "LOGNAME": true, "SHELL": true,
	// Network setup the machine may need to reach GitHub at all.
	"http_proxy": true, "HTTP_PROXY": true, "https_proxy": true, "HTTPS_PROXY": true,
	"no_proxy": true, "NO_PROXY": true, "all_proxy": true, "ALL_PROXY": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true,
	"REQUESTS_CA_BUNDLE": true, "CURL_CA_BUNDLE": true,
	// Machine-level tool selection, not personal state.
	"DEVELOPER_DIR": true, "DOCKER_HOST": true, "XDG_RUNTIME_DIR": true,
}

// JobEnv builds the environment for a runner from base (spareci's own
// environment). With jobHome empty, base is returned unchanged. Otherwise
// HOME points at jobHome, only passEnv and LC_* survive, and PATH loses the
// entries inside userHome (~/.cargo/bin, ~/.nvm/..., ~/go/bin), whose tools
// would look for their state in the real home or fail without it.
//
// Tools that derive their caches from HOME (Go, npm, pip, cargo, rustup, XDG
// defaults) then use jobHome without further variables.
func JobEnv(base []string, userHome, jobHome string) []string {
	if jobHome == "" {
		return base
	}
	env := make([]string, 0, len(passEnv)+4)
	for _, kv := range base {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !(passEnv[k] || strings.HasPrefix(k, "LC_")) {
			continue
		}
		if k == "PATH" {
			v = stripHome(v, userHome)
		}
		env = append(env, k+"="+v)
	}
	return append(env,
		"HOME="+jobHome,
		// The system gitconfig (Xcode's, Homebrew's) turns on the macOS
		// keychain credential helper. Jobs authenticate with their own token.
		"GIT_CONFIG_NOSYSTEM=1",
	)
}

// stripHome drops PATH entries in or under home, and relative ones.
func stripHome(path, home string) string {
	home = filepath.Clean(home)
	var keep []string
	for _, p := range filepath.SplitList(path) {
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		p = filepath.Clean(p)
		if home != "" && home != "/" && (p == home || strings.HasPrefix(p, home+string(filepath.Separator))) {
			continue
		}
		keep = append(keep, p)
	}
	if len(keep) == 0 {
		return "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	}
	return strings.Join(keep, string(filepath.ListSeparator))
}

// prepareHome creates the job home on first use. It is private to the user
// because jobs may leave credentials in it (a docker login, an npm token).
func prepareHome(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("job home: %w", err)
	}
	return os.Chmod(dir, 0o700)
}
