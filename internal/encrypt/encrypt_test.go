package encrypt

import (
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

func TestRoundTripAndWrongKeyFails(testState *testing.T) {
	dir := testState.TempDir()
	id, _ := age.GenerateX25519Identity()
	other, _ := age.GenerateX25519Identity()
	src := filepath.Join(dir, "a.dump")
	if err := os.WriteFile(src, []byte("secret data"), 0o600); err != nil {
		testState.Fatal(err)
	}
	enc := src + ".age"
	if err := File(src, enc, id.Recipient().String()); err != nil {
		testState.Fatal(err)
	}
	if raw, _ := os.ReadFile(enc); string(raw) == "secret data" {
		testState.Fatal("file was not encrypted")
	}
	out := filepath.Join(dir, "out")
	if err := Decrypt(enc, out, id.String()); err != nil {
		testState.Fatal(err)
	}
	if got, _ := os.ReadFile(out); string(got) != "secret data" {
		testState.Fatalf("got %q", got)
	}
	bad := filepath.Join(dir, "bad")
	if err := Decrypt(enc, bad, other.String()); err == nil {
		testState.Fatal("wrong key decrypted")
	}
	if _, err := os.Stat(bad); err == nil {
		testState.Fatal("partial output left behind")
	}
	if err := ParseRecipient("nope"); err == nil {
		testState.Fatal("bad recipient accepted")
	}
}
