// Package ghapp creates the GitHub App the watchdog workflow uses to write
// repo variables, through GitHub's App Manifest flow: the user confirms the
// app in the browser, GitHub redirects back to a localhost listener with a
// one-time code, and the code is exchanged for the app's ID and private key.
// Nothing is hosted, and the workflow mints an hour-long installation token
// on each run, so there is no long-lived token to expire.
package ghapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Row-Labs/spareci/internal/route"
)

// Secret names the workflow reads. The client ID isn't sensitive, but keeping
// both as secrets means one place to look and one command to remove them.
const (
	ClientIDSecret = route.AppClientIDSecret
	KeySecret      = route.AppKeySecret
)

// App is what GitHub returns when a manifest code is converted.
type App struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	ClientID string `json:"client_id"`
	PEM      string `json:"pem"`
	HTMLURL  string `json:"html_url"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// Target is the repo the app is for.
type Target struct {
	Repo      string // owner/name
	RepoID    int64
	RepoURL   string
	Owner     string
	OwnerID   int64
	OwnerType string // "User" or "Organization"
}

// Name is the app's name: GitHub needs it unique across all apps and at most
// 34 characters, so it carries a random suffix.
func Name(repo string, suffix string) string {
	name := repo
	if i := strings.IndexByte(repo, '/'); i >= 0 {
		name = repo[i+1:]
	}
	const max = 34
	room := max - len("spareci--") - len(suffix)
	if len(name) > room {
		name = strings.TrimRight(name[:room], "-_.")
	}
	return "spareci-" + name + "-" + suffix
}

// VariablesPermission is GitHub's key for the repository permission titled
// "Variables" (Actions variables): read lists them, write deletes them.
const VariablesPermission = "actions_variables"

// Manifest is the app definition. It asks for Variables: write and nothing
// else (metadata: read comes with every app). Cancelling and re-running stays
// with the workflow's own GITHUB_TOKEN. No webhook, and the app is private,
// so only its owner can install it.
func Manifest(t Target, name, base string) map[string]any {
	return map[string]any{
		"name":         name,
		"url":          t.RepoURL,
		"description":  fmt.Sprintf("Lets the spareci watchdog workflow in %s clear its routing variables when the machine running spareci dies. Created by `spareci watchdog --setup`.", t.Repo),
		"public":       false,
		"redirect_url": base + "/created",
		"setup_url":    base + "/installed",
		"hook_attributes": map[string]any{
			"url":    t.RepoURL,
			"active": false,
		},
		"default_permissions": map[string]string{VariablesPermission: "write"},
		"default_events":      []string{},
	}
}

// NewAppURL is where the manifest is POSTed. A private app can only be
// installed on the account that owns it, so an organization's repo needs an
// app owned by the organization.
func NewAppURL(web string, t Target, state string) string {
	path := "/settings/apps/new"
	if t.OwnerType == "Organization" {
		path = "/organizations/" + url.PathEscape(t.Owner) + "/settings/apps/new"
	}
	return web + path + "?state=" + url.QueryEscape(state)
}

// InstallURL opens the install page with the account and repo preselected.
func InstallURL(web, slug string, t Target) string {
	return fmt.Sprintf("%s/apps/%s/installations/new/permissions?suggested_target_id=%d&repository_ids[]=%d",
		web, url.PathEscape(slug), t.OwnerID, t.RepoID)
}

// API is GitHub's REST API as the app (not as the user).
type API struct {
	Base string // https://api.github.com
	HTTP *http.Client
}

func (a API) call(ctx context.Context, method, path, bearer string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, a.Base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "spareci")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	hc := a.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return &StatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string { return fmt.Sprintf("github api %d: %s", e.Status, e.Body) }

// Convert exchanges the one-time manifest code (valid for an hour) for the
// app's credentials. The private key is only ever returned here.
func (a API) Convert(ctx context.Context, code string) (App, error) {
	var app App
	err := a.call(ctx, "POST", "/app-manifests/"+url.PathEscape(code)+"/conversions", "", &app)
	if err == nil && (app.ClientID == "" || app.PEM == "") {
		err = errors.New("manifest conversion returned no client ID or key")
	}
	return app, err
}

// RepoInstallation is the app's installation that covers repo. It is a 404
// until the user installs the app on that repo.
func (a API) RepoInstallation(ctx context.Context, jwt, repo string) (int64, error) {
	var inst struct {
		ID int64 `json:"id"`
	}
	err := a.call(ctx, "GET", "/repos/"+repo+"/installation", jwt, &inst)
	return inst.ID, err
}

// InstallationToken mints an hour-long token for an installation, the same
// thing actions/create-github-app-token does in the workflow.
func (a API) InstallationToken(ctx context.Context, jwt string, installation int64) (string, error) {
	var tok struct {
		Token string `json:"token"`
	}
	err := a.call(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", installation), jwt, &tok)
	return tok.Token, err
}

// CanReadVariables checks a token against GET /repos/{repo}/actions/variables,
// which needs the Variables permission and changes nothing.
func (a API) CanReadVariables(ctx context.Context, token, repo string) error {
	return a.call(ctx, "GET", "/repos/"+repo+"/actions/variables?per_page=1", token, nil)
}

// JWT signs the short-lived token an app uses to call /app endpoints. GitHub
// accepts the client ID as the issuer; iat is backdated for clock drift.
func JWT(issuer, keyPEM string, now time.Time) (string, error) {
	key, err := parseKey(keyPEM)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": issuer,
	})
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

func parseKey(keyPEM string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		return nil, errors.New("app private key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("app private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("app private key is not RSA")
	}
	return rk, nil
}

// formPage is served at / and POSTs the manifest to GitHub straight away: the
// manifest flow has to start with a form POST from the user's browser.
func formPage(action string, manifest map[string]any) []byte {
	m, _ := json.Marshal(manifest)
	var b bytes.Buffer
	fmt.Fprintf(&b, `<!doctype html><meta charset="utf-8"><title>spareci watchdog setup</title>
<body style="font-family:system-ui;margin:3rem">
<form id="f" method="post" action="%s">
<input type="hidden" name="manifest" value="%s">
<p>Sending you to GitHub to create the spareci watchdog app…</p>
<button type="submit">Continue to GitHub</button>
</form>
<script>document.getElementById("f").submit()</script>`, htmlEscape(action), htmlEscape(string(m)))
	return b.Bytes()
}

func donePage(msg string) []byte {
	return []byte(`<!doctype html><meta charset="utf-8"><title>spareci watchdog setup</title>
<body style="font-family:system-ui;margin:3rem"><p>` + htmlEscape(msg) + `</p>`)
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}
