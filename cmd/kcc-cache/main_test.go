package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

func newCred(token string) ClientAuthentication {
	c := ClientAuthentication{
		APIVersion: "client.authentication.k8s.io/v1",
		Kind:       "ExecCredential",
	}
	c.Status.ExpirationTimestamp = time.Now().Add(time.Hour)
	c.Status.Token = token
	return c
}

func testBackendRoundTrip(t *testing.T, b Backend) {
	t.Helper()

	if _, ok, err := b.Get("missing"); err != nil || ok {
		t.Fatalf("Get(missing) = ok:%v err:%v, want ok:false err:nil", ok, err)
	}

	want := newCred("secret-token")
	if err := b.Set("key1", want); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, ok, err := b.Get("key1")
	if err != nil || !ok {
		t.Fatalf("Get(key1) = ok:%v err:%v, want ok:true err:nil", ok, err)
	}
	if got.Status.Token != want.Status.Token {
		t.Fatalf("token = %q, want %q", got.Status.Token, want.Status.Token)
	}

	// overwrite
	if err := b.Set("key1", newCred("rotated")); err != nil {
		t.Fatalf("Set(overwrite): %v", err)
	}
	got, _, _ = b.Get("key1")
	if got.Status.Token != "rotated" {
		t.Fatalf("token after overwrite = %q, want %q", got.Status.Token, "rotated")
	}
}

func TestKeyringBackend(t *testing.T) {
	keyring.MockInit()
	testBackendRoundTrip(t, keyringBackend{})
}

func TestFileBackend(t *testing.T) {
	dir := t.TempDir()
	testBackendRoundTrip(t, fileBackend{path: filepath.Join(dir, "nested", "cache.json")})
}

func TestFileBackendCleansExpired(t *testing.T) {
	dir := t.TempDir()
	b := fileBackend{path: filepath.Join(dir, "cache.json")}

	expired := newCred("old")
	expired.Status.ExpirationTimestamp = time.Now().Add(-time.Hour)
	if err := b.Set("expired", expired); err != nil {
		t.Fatalf("Set(expired): %v", err)
	}
	// writing another entry triggers cleanup of expired ones
	if err := b.Set("fresh", newCred("new")); err != nil {
		t.Fatalf("Set(fresh): %v", err)
	}

	if _, ok, _ := b.Get("expired"); ok {
		t.Fatal("expired entry should have been removed on Set")
	}
	if _, ok, _ := b.Get("fresh"); !ok {
		t.Fatal("fresh entry should be present")
	}
}
