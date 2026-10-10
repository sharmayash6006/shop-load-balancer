package auth

import (
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	s := []byte("0123456789abcdef")
	tok := Sign(Claims{Sub: 5, Name: "Yash", Exp: time.Now().Add(time.Hour).Unix()}, s)
	c, err := Verify(tok, s)
	if err != nil || c.Sub != 5 || c.Name != "Yash" {
		t.Fatalf("verify: %v %+v", err, c)
	}
	if _, err := Verify(tok, []byte("another-secret-xx")); err == nil {
		t.Fatal("wrong secret must fail")
	}
	if _, err := Verify(tok[:len(tok)-2]+"xx", s); err == nil {
		t.Fatal("tampered signature must fail")
	}
	if _, err := Verify(Sign(Claims{Sub: 1, Exp: time.Now().Add(-time.Minute).Unix()}, s), s); err == nil {
		t.Fatal("expired token must fail")
	}
}
