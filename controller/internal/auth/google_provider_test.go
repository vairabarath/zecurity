package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestGoogleClaimsToAuthContext(t *testing.T) {
	issued := time.Unix(1_700_000_000, 0)
	c := &GoogleClaims{
		Email:         "user@example.com",
		EmailVerified: true,
		Name:          "Test User",
		Sub:           "google-sub-123",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:   "https://accounts.google.com",
			IssuedAt: jwt.NewNumericDate(issued),
		},
	}

	ac := googleClaimsToAuthContext(c)

	if ac.Provider != "google" {
		t.Errorf("Provider: got %q want google", ac.Provider)
	}
	if ac.Issuer != "https://accounts.google.com" {
		t.Errorf("Issuer: got %q", ac.Issuer)
	}
	if ac.Subject != "google-sub-123" {
		t.Errorf("Subject: got %q", ac.Subject)
	}
	if ac.Email != "user@example.com" {
		t.Errorf("Email: got %q", ac.Email)
	}
	if ac.Name != "Test User" {
		t.Errorf("Name: got %q", ac.Name)
	}
	if !ac.EmailVerified {
		t.Error("EmailVerified: got false want true")
	}
	if ac.AuthTime != issued.Unix() {
		t.Errorf("AuthTime: got %d want %d", ac.AuthTime, issued.Unix())
	}
}

func TestGoogleClaimsToAuthContext_NilIssuedAt(t *testing.T) {
	c := &GoogleClaims{Sub: "s", Email: "e@x.com", EmailVerified: true}
	ac := googleClaimsToAuthContext(c)
	if ac.AuthTime != 0 {
		t.Errorf("AuthTime with nil IssuedAt: got %d want 0", ac.AuthTime)
	}
	if ac.Provider != "google" || ac.Subject != "s" {
		t.Errorf("unexpected mapping: %+v", ac)
	}
}
