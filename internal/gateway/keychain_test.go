package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRealKeychainStoreRoundTrip(t *testing.T) {
	if os.Getenv("TIDEMUX_REAL_KEYCHAIN_TEST") != "1" {
		t.Skip("opt-in real Keychain test with synthetic credential")
	}
	nonce := make([]byte, 16)
	rand.Read(nonce)
	ref := KeychainReference{Service: "com.tidemux.test.temporary", Account: hex.EncodeToString(nonce)}
	path := filepath.Join(t.TempDir(), "test.keychain-db")
	if out, err := exec.Command("security", "create-keychain", "-p", "synthetic-test-password", path).CombinedOutput(); err != nil {
		t.Fatalf("temporary keychain creation: %v %s", err, out)
	}
	defer exec.Command("security", "delete-keychain", path).Run()
	if err := exec.Command("security", "unlock-keychain", "-p", "synthetic-test-password", path).Run(); err != nil {
		t.Fatal("temporary keychain unlock failed")
	}
	store := MacOSKeychain{path: path}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	defer store.Delete(context.Background(), ref)
	secret := "synthetic-test-credential-not-an-api-key"
	if err := store.StoreNew(ctx, ref, secret); err != nil {
		t.Fatal(err)
	}
	got, err := store.Lookup(ctx, ref)
	if err != nil || got != secret {
		t.Fatal("Keychain roundtrip mismatch")
	}
	if err = store.StoreNew(ctx, ref, "must-not-replace-existing-item"); err == nil {
		t.Fatal("existing item should not be replaced")
	}
	got, err = store.Lookup(ctx, ref)
	if err != nil || got != secret {
		t.Fatal("existing item changed")
	}
}
