package auth

import (
	"errors"
	"testing"
	"time"
)

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "correct horse battery staple" {
		t.Fatal("hash equals plaintext")
	}

	match, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil || !match {
		t.Fatalf("correct password: match=%v err=%v", match, err)
	}

	match, err = VerifyPassword("wrong password", hash)
	if err != nil {
		t.Fatalf("wrong password verify errored: %v", err)
	}
	if match {
		t.Fatal("wrong password matched")
	}
}

func TestGenerateParseTokenRoundTrip(t *testing.T) {
	token, err := GenerateToken("user-123", "a@b.com", true, "secret", time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	claims, err := ParseToken(token, "secret")
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if claims.Subject != "user-123" || claims.Email != "a@b.com" || !claims.IsAdmin {
		t.Errorf("claims = %+v", claims)
	}
	if claims.ExpiresAt == nil || time.Until(claims.ExpiresAt.Time) > time.Hour {
		t.Errorf("exp = %v", claims.ExpiresAt)
	}
	if claims.IssuedAt == nil {
		t.Error("iat missing")
	}
}

func TestParseTokenExpired(t *testing.T) {
	token, err := GenerateToken("user-123", "a@b.com", false, "secret", -time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := ParseToken(token, "secret"); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expected ErrTokenExpired, got %v", err)
	}
}

func TestParseTokenWrongSecret(t *testing.T) {
	token, err := GenerateToken("user-123", "a@b.com", false, "secret", time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := ParseToken(token, "other-secret"); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("expected ErrTokenInvalid, got %v", err)
	}
}

func TestParseTokenGarbage(t *testing.T) {
	if _, err := ParseToken("not.a.jwt", "secret"); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("expected ErrTokenInvalid, got %v", err)
	}
}
