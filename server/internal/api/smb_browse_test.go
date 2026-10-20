package api

import (
	"strings"
	"testing"

	"github.com/EJM-Digital/Xuva/server/internal/secret"
)

func TestSMBCredentialSealOpenRoundTrip(t *testing.T) {
	store, err := secret.New(t.TempDir())
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	const user = `WORKGROUP\media`
	const pass = `p@ss w/ spaces & symbols #1`

	encoded, err := sealSMBCredential(store, user, pass)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if strings.Contains(encoded, user) || strings.Contains(encoded, pass) {
		t.Fatal("encoded credential leaks plaintext")
	}

	gotUser, gotPass, err := openSMBCredential(store, encoded)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if gotUser != user || gotPass != pass {
		t.Errorf("round trip mismatch: got (%q,%q) want (%q,%q)", gotUser, gotPass, user, pass)
	}
}

func TestSMBCredentialNilStore(t *testing.T) {
	if _, err := sealSMBCredential(nil, "u", "p"); err == nil {
		t.Error("expected error sealing with nil store")
	}
	if _, _, err := openSMBCredential(nil, "blob"); err == nil {
		t.Error("expected error opening with nil store")
	}
}

func TestSMBCredentialOpenRejectsGarbage(t *testing.T) {
	store, err := secret.New(t.TempDir())
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	if _, _, err := openSMBCredential(store, "not-valid-base64!!!"); err == nil {
		t.Error("expected error on non-base64 input")
	}
	if _, _, err := openSMBCredential(store, "YWJjZGVm"); err == nil {
		t.Error("expected error opening a blob this store never sealed")
	}
}
