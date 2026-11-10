package ghapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func testKey(t *testing.T) (*rsa.PrivateKey, string) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	return k, string(p)
}

func verifyJWT(t *testing.T, tok string, pub *rsa.PublicKey) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("jwt signature: %v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	json.Unmarshal(raw, &claims)
	return claims
}

func TestJWT(t *testing.T) {
	k, p := testKey(t)
	now := time.Unix(1_800_000_000, 0)
	tok, err := JWT("Iv23abc", p, now)
	if err != nil {
		t.Fatal(err)
	}
	c := verifyJWT(t, tok, &k.PublicKey)
	if c["iss"] != "Iv23abc" || c["iat"].(float64) != float64(now.Unix()-60) || c["exp"].(float64) > float64(now.Unix()+600) {
		t.Errorf("claims = %v", c)
	}
	// PKCS#8 keys parse too.
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	if _, err := JWT("x", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), now); err != nil {
		t.Error(err)
	}
	if _, err := JWT("x", "not a key", now); err == nil {
		t.Error("garbage key accepted")
	}
}

func TestName(t *testing.T) {
	if got := Name("Row-Labs/spareci", "1a2b"); got != "spareci-spareci-1a2b" {
		t.Errorf("Name = %q", got)
	}
	long := Name("o/a-really-long-repository-name-that-goes-on", "1a2b")
	if len(long) > 34 || !strings.HasPrefix(long, "spareci-a-really") || !strings.HasSuffix(long, "-1a2b") {
		t.Errorf("long Name = %q (%d)", long, len(long))
	}
}

func TestURLs(t *testing.T) {
	user := Target{Owner: "me", OwnerType: "User", OwnerID: 7, RepoID: 9}
	org := Target{Owner: "Row-Labs", OwnerType: "Organization", OwnerID: 7, RepoID: 9}
	if got := NewAppURL("https://github.com", user, "s"); got != "https://github.com/settings/apps/new?state=s" {
		t.Error(got)
	}
	if got := NewAppURL("https://github.com", org, "s"); got != "https://github.com/organizations/Row-Labs/settings/apps/new?state=s" {
		t.Error(got)
	}
	if got := InstallURL("https://github.com", "spareci-x", org); got != "https://github.com/apps/spareci-x/installations/new/permissions?suggested_target_id=7&repository_ids[]=9" {
		t.Error(got)
	}
}

type fakeSecrets struct {
	mu  sync.Mutex
	got map[string]string
}

func (f *fakeSecrets) SetSecret(_ context.Context, repo, name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got[repo+"/"+name] = value
	return nil
}

// TestSetupFlow drives the whole manifest flow against a fake GitHub: the
// "browser" loads the local page, submits the manifest, follows GitHub's
// redirect back with a code, and installs the app after a few polls.
func TestSetupFlow(t *testing.T) {
	key, keyPEM := testKey(t)
	target := Target{Repo: "o/r", RepoID: 9, RepoURL: "https://github.com/o/r", Owner: "o", OwnerID: 7, OwnerType: "User"}

	var mu sync.Mutex
	var manifest map[string]any
	installed, polls := false, 0

	gh := http.NewServeMux()
	// Web side.
	gh.HandleFunc("POST /settings/apps/new", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		json.Unmarshal([]byte(r.PostForm.Get("manifest")), &manifest)
		redirect := manifest["redirect_url"].(string)
		mu.Unlock()
		http.Redirect(w, r, redirect+"?code=c0de&state="+url.QueryEscape(r.URL.Query().Get("state")), http.StatusFound)
	})
	gh.HandleFunc("GET /apps/spareci-r/installations/new/permissions", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("suggested_target_id") != "7" || r.URL.Query().Get("repository_ids[]") != "9" {
			t.Errorf("install page query = %s", r.URL.RawQuery)
		}
		io.WriteString(w, "install page")
	})
	// API side.
	gh.HandleFunc("POST /app-manifests/c0de/conversions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("conversion should be unauthenticated")
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 1, "slug": "spareci-r", "name": "spareci-r", "client_id": "Iv23x", "pem": keyPEM, "html_url": "https://github.com/apps/spareci-r"})
	})
	gh.HandleFunc("GET /repos/o/r/installation", func(w http.ResponseWriter, r *http.Request) {
		c := verifyJWT(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), &key.PublicKey)
		if c["iss"] != "Iv23x" {
			t.Errorf("iss = %v", c["iss"])
		}
		mu.Lock()
		defer mu.Unlock()
		polls++
		if polls == 3 {
			installed = true // the user clicked Install
		}
		if !installed {
			http.Error(w, `{"message":"Not Found"}`, 404)
			return
		}
		io.WriteString(w, `{"id": 55}`)
	})
	gh.HandleFunc("POST /app/installations/55/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"token": "ghs_inst"}`)
	})
	gh.HandleFunc("GET /repos/o/r/actions/variables", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghs_inst" {
			http.Error(w, "bad token", 401)
			return
		}
		io.WriteString(w, `{"total_count":0,"variables":[]}`)
	})
	srv := httptest.NewServer(gh)
	defer srv.Close()

	browser := func(start string) error {
		go func() {
			resp, err := http.Get(start)
			if err != nil {
				t.Error(err)
				return
			}
			page, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			m := regexp.MustCompile(`action="([^"]+)"[\s\S]*name="manifest" value="([^"]+)"`).FindSubmatch(page)
			if m == nil {
				t.Errorf("no manifest form in %s", page)
				return
			}
			// Submitting follows GitHub's redirect to /created, then on to the install page.
			resp, err = http.PostForm(html.UnescapeString(string(m[1])), url.Values{"manifest": {html.UnescapeString(string(m[2]))}})
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			if !strings.HasSuffix(resp.Request.URL.Path, "/installations/new/permissions") {
				t.Errorf("ended at %s", resp.Request.URL)
			}
		}()
		return nil
	}

	secrets := &fakeSecrets{got: map[string]string{}}
	var out strings.Builder
	s := &Setup{Target: target, API: API{Base: srv.URL}, Web: srv.URL, Secrets: secrets, Open: browser, Out: &out, Wait: 10 * time.Second, Poll: 10 * time.Millisecond}
	app, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if app.Slug != "spareci-r" {
		t.Errorf("app = %+v", app)
	}
	if secrets.got["o/r/"+ClientIDSecret] != "Iv23x" || secrets.got["o/r/"+KeySecret] != keyPEM {
		t.Errorf("secrets = %v", secrets.got)
	}
	perms, _ := manifest["default_permissions"].(map[string]any)
	if len(perms) != 1 || perms["variables"] != "write" || manifest["public"] != false {
		t.Errorf("manifest = %v", manifest)
	}
	if hook, _ := manifest["hook_attributes"].(map[string]any); hook["active"] != false {
		t.Errorf("webhook should be off: %v", hook)
	}
	if !strings.Contains(out.String(), "can read o/r's variables") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestSetupRejectsForeignState(t *testing.T) {
	s := &Setup{Target: Target{Repo: "o/r"}, Out: io.Discard, Wait: 300 * time.Millisecond,
		Open: func(start string) error {
			go func() {
				resp, err := http.Get(start + "/created?code=x&state=wrong")
				if err != nil {
					t.Error(err)
					return
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusBadRequest {
					t.Errorf("foreign state got %d", resp.StatusCode)
				}
			}()
			return nil
		}}
	if _, err := s.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "no app was created") {
		t.Errorf("err = %v", err)
	}
}
