package ghapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// SecretWriter stores repo Actions secrets (gh.Client does, sealed to the
// repo's public key).
type SecretWriter interface {
	SetSecret(ctx context.Context, repo, name, value string) error
}

// Setup walks the user through creating and installing the watchdog app, and
// stores its credentials as repo secrets.
type Setup struct {
	Target  Target
	API     API
	Web     string // https://github.com
	Secrets SecretWriter
	Open    func(url string) error // opens the user's browser
	Out     io.Writer
	Wait    time.Duration // how long each browser step may take
	Poll    time.Duration // how often to check for the installation
}

// Run creates the app, stores ClientIDSecret and KeySecret, waits for the app
// to be installed on the repo, and checks that a token minted for the
// installation can read the repo's variables.
func (s *Setup) Run(ctx context.Context) (App, error) {
	wait, poll := s.Wait, s.Poll
	if wait == 0 {
		wait = 10 * time.Minute
	}
	if poll == 0 {
		poll = 2 * time.Second
	}
	state, suffix := randHex(16), randHex(2)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return App{}, err
	}
	base := "http://" + ln.Addr().String()
	manifest := Manifest(s.Target, Name(s.Target.Repo, suffix), base)

	created := make(chan App, 1)
	failed := make(chan error, 1)
	installed := make(chan struct{}, 1)
	var once sync.Once
	var converted App // written once, inside once.Do

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(formPage(NewAppURL(s.Web, s.Target, state), manifest))
	})
	mux.HandleFunc("GET /created", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "This link isn't from the spareci setup that's running. Start again with spareci watchdog --setup.", http.StatusBadRequest)
			return
		}
		var convErr error
		once.Do(func() {
			converted, convErr = s.API.Convert(r.Context(), r.URL.Query().Get("code"))
			if convErr != nil {
				failed <- fmt.Errorf("exchange the manifest code for app credentials: %w", convErr)
				return
			}
			created <- converted
		})
		if convErr != nil || converted.Slug == "" {
			w.Write(donePage("Creating the app failed. See the terminal."))
			return
		}
		http.Redirect(w, r, InstallURL(s.Web, converted.Slug, s.Target), http.StatusFound)
	})
	mux.HandleFunc("GET /installed", func(w http.ResponseWriter, r *http.Request) {
		w.Write(donePage("Installed. You can close this tab; spareci is finishing in the terminal."))
		select {
		case installed <- struct{}{}:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	fmt.Fprintf(s.Out, "Opening %s in your browser. It sends you to GitHub to create a private app with one permission, Variables: read and write.\n", base)
	if s.Open == nil || s.Open(base) != nil {
		fmt.Fprintf(s.Out, "Open %s in a browser on this machine.\n", base)
	}

	var app App
	select {
	case app = <-created:
	case err := <-failed:
		return App{}, err
	case <-time.After(wait):
		return App{}, fmt.Errorf("no app was created within %s", wait)
	case <-ctx.Done():
		return App{}, ctx.Err()
	}
	fmt.Fprintf(s.Out, "Created %s (%s).\n", app.Name, app.HTMLURL)

	// The key is only returned once, so store it before anything else can fail.
	if err := s.Secrets.SetSecret(ctx, s.Target.Repo, ClientIDSecret, app.ClientID); err != nil {
		return app, keyLost(app, err)
	}
	if err := s.Secrets.SetSecret(ctx, s.Target.Repo, KeySecret, app.PEM); err != nil {
		return app, keyLost(app, err)
	}
	fmt.Fprintf(s.Out, "Stored %s and %s as secrets in %s.\n", ClientIDSecret, KeySecret, s.Target.Repo)

	install := InstallURL(s.Web, app.Slug, s.Target)
	fmt.Fprintf(s.Out, "Install it on %s in the browser (or open %s).\n", s.Target.Repo, install)
	id, err := s.waitInstalled(ctx, app, installed, wait, poll)
	if err != nil {
		return app, fmt.Errorf("%w. Install the app at %s; the secrets are already stored", err, install)
	}

	jwt, err := JWT(app.ClientID, app.PEM, time.Now())
	if err != nil {
		return app, err
	}
	tok, err := s.API.InstallationToken(ctx, jwt, id)
	if err != nil {
		return app, fmt.Errorf("mint an installation token: %w", err)
	}
	if err := s.API.CanReadVariables(ctx, tok, s.Target.Repo); err != nil {
		return app, fmt.Errorf("the app's token can't read %s's variables: %w", s.Target.Repo, err)
	}
	fmt.Fprintf(s.Out, "Installed, and a token minted for it can read %s's variables.\n", s.Target.Repo)
	return app, nil
}

func (s *Setup) waitInstalled(ctx context.Context, app App, installed <-chan struct{}, wait, poll time.Duration) (int64, error) {
	deadline := time.After(wait)
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		jwt, err := JWT(app.ClientID, app.PEM, time.Now())
		if err != nil {
			return 0, err
		}
		id, err := s.API.RepoInstallation(ctx, jwt, s.Target.Repo)
		var se *StatusError
		switch {
		case err == nil:
			return id, nil
		case errors.As(err, &se) && se.Status == http.StatusNotFound:
		default:
			return 0, fmt.Errorf("check the app's installation: %w", err)
		}
		select {
		case <-tick.C:
		case <-installed:
		case <-deadline:
			return 0, fmt.Errorf("the app wasn't installed on %s within %s", s.Target.Repo, wait)
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

func keyLost(app App, err error) error {
	return fmt.Errorf("created %s, but storing its credentials failed: %w. Generate a new private key at %s and run: gh secret set %s < key.pem; gh secret set %s --body %s",
		app.Name, err, app.HTMLURL, KeySecret, ClientIDSecret, app.ClientID)
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
