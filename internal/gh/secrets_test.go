package gh

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/crypto/nacl/box"
)

func TestSetSecretSealsToRepoKey(t *testing.T) {
	pub, priv, _ := box.GenerateKey(rand.Reader)
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/o/r/actions/secrets/public-key":
			json.NewEncoder(w).Encode(map[string]string{"key_id": "k1", "key": base64.StdEncoding.EncodeToString(pub[:])})
		case "PUT /repos/o/r/actions/secrets/S":
			json.NewDecoder(r.Body).Decode(&got)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL, "t")
	if err := c.SetSecret(context.Background(), "o/r", "S", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if got["key_id"] != "k1" {
		t.Errorf("key_id = %q", got["key_id"])
	}
	sealed, _ := base64.StdEncoding.DecodeString(got["encrypted_value"])
	plain, ok := box.OpenAnonymous(nil, sealed, pub, priv)
	if !ok || string(plain) != "hunter2" {
		t.Errorf("opened %q, %v", plain, ok)
	}
	if ok, err := c.SecretExists(context.Background(), "o/r", "MISSING"); ok || err != nil {
		t.Errorf("SecretExists = %v, %v", ok, err)
	}
}

func TestSealRejectsBadKey(t *testing.T) {
	if _, err := Seal("c2hvcnQ=", []byte("x")); err == nil {
		t.Error("short key accepted")
	}
}
