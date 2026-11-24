// Package encrypt wraps backups in age (https://age-encryption.org). The host only ever holds the
// public recipient, so a stolen host or bucket key cannot read old backups; the private identity
// is needed only to restore or verify.
package encrypt

import (
	"fmt"
	"io"
	"os"

	"filippo.io/age"
)

// ParseRecipient validates an age public key ("age1...").
func ParseRecipient(recipient string) error {
	_, err := age.ParseX25519Recipient(recipient)
	return err
}

// File encrypts src to dst (mode 0600) for recipient. The caller removes src.
func File(src, dst, recipient string) (err error) {
	rcpt, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return fmt.Errorf("age recipient: %w", err)
	}
	return copyThrough(src, dst, func(out io.Writer) (io.WriteCloser, error) { return age.Encrypt(out, rcpt) })
}

// Decrypt reverses File with the private identity ("AGE-SECRET-KEY-1...").
func Decrypt(src, dst, identity string) error {
	id, err := age.ParseX25519Identity(identity)
	if err != nil {
		return fmt.Errorf("age identity: %w", err)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	plain, err := age.Decrypt(in, id)
	if err != nil {
		return fmt.Errorf("decrypt %s: %w", src, err)
	}
	return writeFile(dst, func(out io.Writer) error { _, err := io.Copy(out, plain); return err })
}

func copyThrough(src, dst string, wrap func(io.Writer) (io.WriteCloser, error)) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	return writeFile(dst, func(out io.Writer) error {
		w, err := wrap(out)
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, in); err != nil {
			return err
		}
		return w.Close()
	})
}

// writeFile creates dst exclusively (0600) and removes it again if fill fails.
func writeFile(dst string, fill func(io.Writer) error) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = fill(out)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
	}
	return err
}
