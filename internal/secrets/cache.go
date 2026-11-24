package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"time"
)

type cacheBody struct {
	Saved   time.Time         `json:"saved"`
	Paths   []string          `json:"paths"`
	Secrets map[string]string `json:"secrets"`
}

// keyed is implemented by sources that can seal a cache.
type keyed interface{ CacheKey() string }

// ResolveCached is Resolve plus a last-good copy on disk, so a provider outage does not stop
// backups. fromCache is true when the provider failed and a copy younger than maxAge was used.
// maxAge <= 0, or a source with no stable credential, means no cache at all.
//
// The file is 0600 and AES-GCM sealed with a key derived from the identity's credential. That keeps
// the secrets from sitting in plaintext (backups of the config dir, stray copies), but anyone who can
// read identity.env can open it, so treat the cache with the same care as identity.env.
func ResolveCached(ctx context.Context, src Source, cachePath string, maxAge time.Duration, paths ...string) (set Set, fromCache bool, err error) {
	set, err = Resolve(ctx, src, paths...)
	k, ok := src.(keyed)
	if maxAge <= 0 || !ok || k.CacheKey() == "" {
		return set, false, err
	}
	if err == nil {
		_ = writeCache(cachePath, k.CacheKey(), cacheBody{Saved: time.Now(), Paths: paths, Secrets: set}) // best effort
		return set, false, nil
	}
	body, cerr := readCache(cachePath, k.CacheKey())
	if cerr != nil || time.Since(body.Saved) > maxAge || !slices.Equal(body.Paths, paths) {
		return nil, false, err
	}
	return Set(body.Secrets), true, nil
}

func sealKey(identity string) []byte {
	sum := sha256.Sum256([]byte("backstash-secrets-cache\x00" + identity))
	return sum[:]
}

func writeCache(path, identity string, body cacheBody) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(sealKey(identity))
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, gcm.Seal(nonce, nonce, raw, nil), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readCache(path, identity string) (cacheBody, error) {
	var body cacheBody
	raw, err := os.ReadFile(path)
	if err != nil {
		return body, err
	}
	block, err := aes.NewCipher(sealKey(identity))
	if err != nil {
		return body, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return body, err
	}
	if len(raw) < gcm.NonceSize() {
		return body, errors.New("secrets cache is truncated")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return body, err // wrong key (rotated identity) or tampered file
	}
	return body, json.Unmarshal(plain, &body)
}
