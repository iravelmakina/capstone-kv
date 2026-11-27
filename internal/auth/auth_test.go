package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeKeys(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckAndScopes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	writeKeys(t, path, `[{"name":"mcp","sha256":"`+Hash("secret-1")+`","scopes":["read","write"]}]`)
	k, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := k.Check("secret-1")
	if !ok || key.Name != "mcp" {
		t.Fatalf("valid key: %+v, %v", key, ok)
	}
	if !key.Allows(Read) || !key.Allows(Write) || key.Allows(Delete) || key.Allows(Admin) {
		t.Fatalf("scopes wrong: %v", key.Scopes)
	}
	if _, ok := k.Check("secret-2"); ok {
		t.Fatal("unknown key accepted")
	}
	if _, ok := k.Check(""); ok {
		t.Fatal("empty key accepted")
	}
}

func TestBearer(t *testing.T) {
	for in, want := range map[string]string{
		"Bearer abc": "abc", "bearer abc": "abc", "Bearer  abc ": "abc",
		"Basic abc": "", "Bearer": "", "": "",
	} {
		if got := Bearer(in); got != want {
			t.Errorf("Bearer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMissingOrInvalidFileRefused(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(filepath.Join(dir, "none.json")); err == nil {
		t.Fatal("missing file accepted")
	}
	bad := filepath.Join(dir, "bad.json")
	writeKeys(t, bad, `[{"name":"x","sha256":"short","scopes":["read"]}]`)
	if _, err := Load(bad); err == nil {
		t.Fatal("bad hash accepted")
	}
}

// Spec "Key rotated without a restart": a key removed from the file stops working within
// the watch interval; the others keep working; an invalid file keeps the old keys.
func TestWatchReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	two := `[{"name":"a","sha256":"` + Hash("ka") + `","scopes":["read"]},{"name":"b","sha256":"` + Hash("kb") + `","scopes":["read"]}]`
	writeKeys(t, path, two)
	k, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go k.Watch(ctx, 10*time.Millisecond)

	writeKeys(t, path, `[{"name":"b","sha256":"`+Hash("kb")+`","scopes":["read"]}]`)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := k.Check("ka"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("removed key still accepted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := k.Check("kb"); !ok {
		t.Fatal("remaining key rejected")
	}
	writeKeys(t, path, `not json at all`)
	time.Sleep(100 * time.Millisecond)
	if _, ok := k.Check("kb"); !ok {
		t.Fatal("an invalid file dropped the loaded keys")
	}
}
