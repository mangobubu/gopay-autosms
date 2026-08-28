package secure

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestBoxRoundTrip(t *testing.T) {
	box, err := New("persistent-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := box.Seal([]byte("147258"))
	if err != nil {
		t.Fatal(err)
	}
	if string(ciphertext) == "147258" {
		t.Fatal("ciphertext contains plaintext")
	}
	plain, err := box.Open(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(plain); got != "147258" {
		t.Fatalf("round-trip = %q", got)
	}
}

func TestBlindIndexIsKeyedDeterministicAndPurposeSeparated(t *testing.T) {
	box, err := New("persistent-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	value := []byte("+628123456789")
	first := box.BlindIndex("account-phone/v1", value)
	if second := box.BlindIndex("account-phone/v1", value); first != second {
		t.Fatalf("blind index is not deterministic: %q != %q", first, second)
	}
	if len(first) != sha256.Size*2 {
		t.Fatalf("blind index length = %d, want %d", len(first), sha256.Size*2)
	}
	if _, err = hex.DecodeString(first); err != nil {
		t.Fatalf("blind index is not hexadecimal: %v", err)
	}
	if first == box.BlindIndex("activation-phone/v1", value) {
		t.Fatal("different purposes shared a blind index")
	}
	other, err := New("different-persistent-secret")
	if err != nil {
		t.Fatal(err)
	}
	if first == other.BlindIndex("account-phone/v1", value) {
		t.Fatal("different keys shared a blind index")
	}
	raw := sha256.Sum256(value)
	if first == hex.EncodeToString(raw[:]) {
		t.Fatal("blind index degraded to an unkeyed SHA-256 digest")
	}
}
