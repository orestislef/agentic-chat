package auth

import (
	"strings"
	"testing"
)

func TestPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("unexpected encoding: %s", hash)
	}
	if !VerifyPassword(hash, "correct horse battery") {
		t.Fatal("right password rejected")
	}
	if VerifyPassword(hash, "wrong horse battery") {
		t.Fatal("wrong password accepted")
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "plain", "$argon2id$v=19$m=1$x$y", "$bcrypt$a$b$c$d"} {
		if VerifyPassword(bad, "pw") {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestTokensAreUniqueAndPrefixed(t *testing.T) {
	a, _ := NewToken("ac")
	b, _ := NewToken("ac")
	if a == b || !strings.HasPrefix(a, "ac_") {
		t.Fatalf("bad tokens: %s %s", a, b)
	}
	if HashToken(a) == HashToken(b) || HashToken(a) != HashToken(a) {
		t.Fatal("hash is not a stable function of the token")
	}
}
