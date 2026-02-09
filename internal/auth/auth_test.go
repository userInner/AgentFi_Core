package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testService creates a Service with a known secret for testing JWT logic
// without requiring Redis (nonce/SIWE tests need Redis and are integration-level).
func testService() *Service {
	return &Service{
		jwtSecret: []byte("test-secret-key"),
		jwtTTL:    24 * time.Hour,
		nonceTTL:  5 * time.Minute,
	}
}

func TestIssueAndValidateJWT(t *testing.T) {
	svc := testService()
	address := "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"

	token, err := svc.issueJWT(address)
	if err != nil {
		t.Fatalf("issueJWT: %v", err)
	}

	claims, err := svc.ValidateJWT(context.Background(), token)
	if err != nil {
		t.Fatalf("ValidateJWT: %v", err)
	}

	if claims.Address != address {
		t.Errorf("address = %q, want %q", claims.Address, address)
	}
	if claims.IssuedAt.IsZero() {
		t.Error("IssuedAt should not be zero")
	}
	if claims.ExpiresAt.IsZero() {
		t.Error("ExpiresAt should not be zero")
	}
	// Expiration should be ~24h from now.
	diff := claims.ExpiresAt.Sub(claims.IssuedAt)
	if diff < 23*time.Hour || diff > 25*time.Hour {
		t.Errorf("token TTL = %v, want ~24h", diff)
	}
}

func TestValidateJWT_Expired(t *testing.T) {
	svc := testService()
	// Create an already-expired token.
	now := time.Now().Add(-48 * time.Hour)
	claims := jwt.MapClaims{
		"sub": "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045",
		"iat": jwt.NewNumericDate(now),
		"exp": jwt.NewNumericDate(now.Add(24 * time.Hour)), // expired 24h ago
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, _ := token.SignedString(svc.jwtSecret)

	_, err := svc.ValidateJWT(context.Background(), tokenStr)
	if err != ErrInvalidToken {
		t.Errorf("expected ErrInvalidToken, got %v", err)
	}
}

func TestValidateJWT_Malformed(t *testing.T) {
	svc := testService()
	_, err := svc.ValidateJWT(context.Background(), "not-a-jwt")
	if err != ErrInvalidToken {
		t.Errorf("expected ErrInvalidToken, got %v", err)
	}
}

func TestValidateJWT_WrongSecret(t *testing.T) {
	svc := testService()
	// Sign with a different secret.
	claims := jwt.MapClaims{
		"sub": "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045",
		"iat": jwt.NewNumericDate(time.Now()),
		"exp": jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, _ := token.SignedString([]byte("wrong-secret"))

	_, err := svc.ValidateJWT(context.Background(), tokenStr)
	if err != ErrInvalidToken {
		t.Errorf("expected ErrInvalidToken, got %v", err)
	}
}

func TestJWTMiddleware_NoHeader(t *testing.T) {
	svc := testService()
	handler := svc.JWTMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestJWTMiddleware_ValidToken(t *testing.T) {
	svc := testService()
	tokenStr, _ := svc.issueJWT("0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045")

	var gotClaims *UserClaims
	handler := svc.JWTMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClaims = ClaimsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tokenStr)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if gotClaims == nil || gotClaims.Address != "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045" {
		t.Errorf("claims address mismatch, got %+v", gotClaims)
	}
}

func TestJWTMiddleware_InvalidToken(t *testing.T) {
	svc := testService()
	handler := svc.JWTMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer invalid-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestClaimsFromContext_NoClaims(t *testing.T) {
	claims := ClaimsFromContext(context.Background())
	if claims != nil {
		t.Errorf("expected nil claims, got %+v", claims)
	}
}
