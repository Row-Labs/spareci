package gh

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"golang.org/x/crypto/nacl/box"
)

// SecretExists reports whether the repo has an Actions secret by that name.
func (c *Client) SecretExists(ctx context.Context, repo, name string) (bool, error) {
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/actions/secrets/%s", repo, name), nil, nil)
	if IsStatus(err, 404) {
		return false, nil
	}
	return err == nil, err
}

// SetSecret creates or updates a repository Actions secret. GitHub only
// accepts values sealed to the repo's public key (a libsodium sealed box), so
// the plaintext never leaves this process.
func (c *Client) SetSecret(ctx context.Context, repo, name, value string) error {
	var key struct {
		KeyID string `json:"key_id"`
		Key   string `json:"key"`
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/actions/secrets/public-key", repo), nil, &key); err != nil {
		return fmt.Errorf("read the repo's public key: %w", err)
	}
	sealed, err := Seal(key.Key, []byte(value))
	if err != nil {
		return err
	}
	return c.do(ctx, "PUT", fmt.Sprintf("/repos/%s/actions/secrets/%s", repo, name), map[string]string{
		"encrypted_value": sealed,
		"key_id":          key.KeyID,
	}, nil)
}

// Seal encrypts value to a base64 Curve25519 public key as a libsodium sealed
// box (crypto_box_seal) and returns it base64-encoded, the format GitHub's
// secrets API takes.
func Seal(publicKey string, value []byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(raw) != 32 {
		return "", fmt.Errorf("repo public key is not a base64 32-byte key")
	}
	var pk [32]byte
	copy(pk[:], raw)
	out, err := box.SealAnonymous(nil, value, &pk, rand.Reader)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(out), nil
}
