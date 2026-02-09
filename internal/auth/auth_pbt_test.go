package auth

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"pgregory.net/rapid"
)

// Feature: agentfi-go-backend, Property 1: JWT round-trip consistency
// For any valid Ethereum address, issuing a JWT token and then validating it
// SHALL return the same address, and the token SHALL contain an issued-at
// timestamp and a 24-hour expiration.
// Validates: Requirements 1.1, 1.2, 1.4
func TestPropertyJWTRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate a random valid Ethereum address (0x + 40 hex chars).
		address := rapid.StringMatching(`0x[a-fA-F0-9]{40}`).Draw(rt, "address")

		// Generate a random JWT secret (8-64 chars).
		secret := rapid.StringMatching(`[a-zA-Z0-9]{8,64}`).Draw(rt, "secret")

		svc := &Service{
			jwtSecret: []byte(secret),
			jwtTTL:    24 * time.Hour,
		}

		// Issue a JWT for the address.
		tokenStr, err := svc.issueJWT(address)
		if err != nil {
			t.Fatalf("issueJWT failed: %v", err)
		}

		// Validate the JWT and extract claims.
		claims, err := svc.ValidateJWT(context.Background(), tokenStr)
		if err != nil {
			t.Fatalf("ValidateJWT failed: %v", err)
		}

		// Round-trip: address must match.
		if claims.Address != address {
			t.Errorf("address mismatch: got %q, want %q", claims.Address, address)
		}

		// IssuedAt must be set and recent.
		if claims.IssuedAt.IsZero() {
			t.Error("IssuedAt should not be zero")
		}

		// ExpiresAt must be set.
		if claims.ExpiresAt.IsZero() {
			t.Error("ExpiresAt should not be zero")
		}

		// Expiration must be 24 hours after issued-at.
		ttl := claims.ExpiresAt.Sub(claims.IssuedAt)
		if ttl < 23*time.Hour+59*time.Minute || ttl > 24*time.Hour+1*time.Minute {
			t.Errorf("TTL = %v, want ~24h", ttl)
		}
	})
}

// Feature: agentfi-go-backend, Property 2: Invalid JWT rejection
// For any expired, malformed, or tampered JWT token, validation SHALL return
// an error and never return valid user claims.
// Validates: Requirements 1.3
func TestPropertyInvalidJWTRejection(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		secret := rapid.StringMatching(`[a-zA-Z0-9]{8,64}`).Draw(rt, "secret")
		address := rapid.StringMatching(`0x[a-fA-F0-9]{40}`).Draw(rt, "address")

		svc := &Service{
			jwtSecret: []byte(secret),
			jwtTTL:    24 * time.Hour,
		}

		// Pick one of three invalid-token strategies.
		strategy := rapid.SampledFrom([]string{"expired", "wrong_secret", "malformed"}).Draw(rt, "strategy")

		var tokenStr string

		switch strategy {
		case "expired":
			// Create a token that expired in the past.
			hoursAgo := rapid.IntRange(25, 720).Draw(rt, "hours_ago")
			past := time.Now().Add(-time.Duration(hoursAgo) * time.Hour)
			claims := jwt.MapClaims{
				"sub": address,
				"iat": jwt.NewNumericDate(past),
				"exp": jwt.NewNumericDate(past.Add(24 * time.Hour)),
			}
			tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
			tokenStr, _ = tok.SignedString(svc.jwtSecret)

		case "wrong_secret":
			// Sign with a different secret.
			wrongSecret := rapid.StringMatching(`[a-zA-Z0-9]{8,64}`).
				Filter(func(s string) bool { return s != secret }).
				Draw(rt, "wrong_secret")
			claims := jwt.MapClaims{
				"sub": address,
				"iat": jwt.NewNumericDate(time.Now()),
				"exp": jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			}
			tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
			tokenStr, _ = tok.SignedString([]byte(wrongSecret))

		case "malformed":
			// Generate a random non-JWT string.
			tokenStr = rapid.StringMatching(`[a-zA-Z0-9]{5,100}`).Draw(rt, "garbage")
		}

		// Validation must fail.
		claims, err := svc.ValidateJWT(context.Background(), tokenStr)
		if err == nil {
			t.Errorf("expected error for %s token, got valid claims: %+v", strategy, claims)
		}
		if claims != nil {
			t.Errorf("expected nil claims for %s token, got: %+v", strategy, claims)
		}
	})
}
