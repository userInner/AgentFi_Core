// Package auth provides SIWE verification and JWT authentication.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	siwe "github.com/spruceid/siwe-go"
)

// Errors returned by the auth service.
var (
	ErrInvalidSignature = errors.New("auth: invalid SIWE signature")
	ErrNonceNotFound    = errors.New("auth: nonce not found or expired")
	ErrInvalidToken     = errors.New("auth: invalid or expired JWT token")
)

// UserClaims holds the authenticated user information extracted from a JWT.
type UserClaims struct {
	Address   string    `json:"address"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Service provides SIWE verification and JWT authentication.
type Service struct {
	rdb       *redis.Client
	jwtSecret []byte
	jwtTTL    time.Duration
	nonceTTL  time.Duration
}

// NewService creates a new auth service.
func NewService(rdb *redis.Client, jwtSecret string) *Service {
	return &Service{
		rdb:       rdb,
		jwtSecret: []byte(jwtSecret),
		jwtTTL:    24 * time.Hour,
		nonceTTL:  5 * time.Minute,
	}
}

// GenerateNonce creates a random nonce and stores it in Redis with a 5-minute TTL.
func (s *Service) GenerateNonce(ctx context.Context) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate nonce: %w", err)
	}
	nonce := hex.EncodeToString(b)

	key := nonceKey(nonce)
	if err := s.rdb.Set(ctx, key, "1", s.nonceTTL).Err(); err != nil {
		return "", fmt.Errorf("auth: store nonce: %w", err)
	}
	return nonce, nil
}

// VerifySIWE parses a SIWE message, verifies the signature, validates the nonce
// against Redis, and issues a JWT on success.
func (s *Service) VerifySIWE(ctx context.Context, message string, signature string) (string, error) {
	// Parse the SIWE message.
	msg, err := siwe.ParseMessage(message)
	if err != nil {
		return "", fmt.Errorf("auth: parse SIWE message: %w", err)
	}

	// Verify the signature against the message.
	_, err = msg.Verify(signature, nil, nil, nil)
	if err != nil {
		return "", ErrInvalidSignature
	}

	// Validate the nonce exists in Redis (one-time use).
	nonce := msg.GetNonce()
	key := nonceKey(nonce)
	res, err := s.rdb.GetDel(ctx, key).Result()
	if err != nil || res == "" {
		return "", ErrNonceNotFound
	}

	// Issue JWT.
	address := msg.GetAddress().String()
	token, err := s.issueJWT(address)
	if err != nil {
		return "", fmt.Errorf("auth: issue JWT: %w", err)
	}
	return token, nil
}

// ValidateJWT verifies a JWT token and returns the user claims.
func (s *Service) ValidateJWT(_ context.Context, tokenStr string) (*UserClaims, error) {
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("auth: unexpected signing method: %v", t.Header["alg"])
		}
		return s.jwtSecret, nil
	})
	if err != nil || !token.Valid {
		return nil, ErrInvalidToken
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, ErrInvalidToken
	}

	address, _ := claims["sub"].(string)
	if address == "" {
		return nil, ErrInvalidToken
	}

	iat, _ := claims.GetIssuedAt()
	exp, _ := claims.GetExpirationTime()
	if iat == nil || exp == nil {
		return nil, ErrInvalidToken
	}

	return &UserClaims{
		Address:   address,
		IssuedAt:  iat.Time,
		ExpiresAt: exp.Time,
	}, nil
}

// issueJWT creates a signed JWT for the given Ethereum address.
func (s *Service) issueJWT(address string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"sub": address,
		"iat": jwt.NewNumericDate(now),
		"exp": jwt.NewNumericDate(now.Add(s.jwtTTL)),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(s.jwtSecret)
}

// nonceKey returns the Redis key for a nonce.
func nonceKey(nonce string) string {
	return "auth:nonce:" + nonce
}

// --- JWT Middleware ---

type contextKey string

const userClaimsKey contextKey = "userClaims"

// JWTMiddleware returns a Chi middleware that validates JWT tokens from the
// Authorization header and injects UserClaims into the request context.
// Invalid or missing tokens result in a 401 response.
func (s *Service) JWTMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if header == "" || !strings.HasPrefix(header, "Bearer ") {
			http.Error(w, `{"error":{"code":"UNAUTHORIZED","message":"missing or invalid authorization header"}}`, http.StatusUnauthorized)
			return
		}

		tokenStr := strings.TrimPrefix(header, "Bearer ")
		claims, err := s.ValidateJWT(r.Context(), tokenStr)
		if err != nil {
			http.Error(w, `{"error":{"code":"UNAUTHORIZED","message":"invalid or expired token"}}`, http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), userClaimsKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ClaimsFromContext extracts UserClaims from the request context.
// Returns nil if no claims are present.
func ClaimsFromContext(ctx context.Context) *UserClaims {
	claims, _ := ctx.Value(userClaimsKey).(*UserClaims)
	return claims
}
