package secure

import "testing"

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
