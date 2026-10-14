package auth

import "testing"

func TestNewTokenUniqueAndLong(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != 64 {
			t.Fatalf("token length = %d, want 64", len(tok))
		}
		if seen[tok] {
			t.Fatalf("duplicate token %q", tok)
		}
		seen[tok] = true
	}
}

func TestHashToken(t *testing.T) {
	h := HashToken("secret")
	if h != HashToken("secret") {
		t.Fatal("hash not deterministic")
	}
	if h == "secret" || h == HashToken("secret2") {
		t.Fatal("hash must differ from input and other inputs")
	}
	// Known SHA-256 of "abc".
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := HashToken("abc"); got != want {
		t.Fatalf("HashToken(abc) = %s, want %s", got, want)
	}
}
