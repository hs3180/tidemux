package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnlockUsesSystemCommandWithoutPasswordArgument(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
 show-keychain-info) test -f "$UNLOCK_STATE" ;;
 unlock-keychain) printf '%s\n' "$@" >> "$UNLOCK_CALLS"; touch "$UNLOCK_STATE" ;;
 *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "security"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("UNLOCK_STATE", filepath.Join(dir, "state"))
	t.Setenv("UNLOCK_CALLS", filepath.Join(dir, "calls"))
	tty, err := os.CreateTemp(dir, "tty")
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()
	for i := 0; i < 2; i++ {
		if err := unlockKeychainIfNeeded(context.Background(), tty); err != nil {
			t.Fatal(err)
		}
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if string(calls) != "unlock-keychain\n" {
		t.Fatalf("unexpected unlock arguments/repeated prompt: %q", calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := unlockKeychainIfNeeded(ctx, tty); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatal("canceled authorization continued")
	}
}
