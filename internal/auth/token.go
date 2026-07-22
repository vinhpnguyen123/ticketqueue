package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrTokenExpired is returned by ParseToken for a valid-but-expired token.
var ErrTokenExpired = errors.New("token expired")

// ErrTokenInvalid is returned by ParseToken for anything else wrong with a token.
var ErrTokenInvalid = errors.New("token invalid")

// Claims are the JWT claims ticketqueue issues.
type Claims struct {
	Email   string `json:"email"`
	IsAdmin bool   `json:"is_admin"`
	jwt.RegisteredClaims
}

// GenerateToken issues an HS256 JWT with sub, email, is_admin, iat, exp.
func GenerateToken(userID, email string, isAdmin bool, secret string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := Claims{
		Email:   email,
		IsAdmin: isAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signed, nil
}

// PurchaseClaims are the claims of a purchase token issued to an admitted
// queue member. The purchase endpoint validates it in Week 3.
type PurchaseClaims struct {
	EventID    string `json:"event_id"`
	AdmittedAt int64  `json:"admitted_at"`
	jwt.RegisteredClaims
}

// GeneratePurchaseToken issues an HS256 JWT proving the user was admitted
// from the queue; it expires when their purchase window closes.
func GeneratePurchaseToken(userID, eventID string, admittedAt time.Time, windowSeconds int, secret string) (string, error) {
	claims := PurchaseClaims{
		EventID:    eventID,
		AdmittedAt: admittedAt.Unix(),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(admittedAt),
			ExpiresAt: jwt.NewNumericDate(admittedAt.Add(time.Duration(windowSeconds) * time.Second)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("sign purchase token: %w", err)
	}
	return signed, nil
}

// ParsePurchaseToken validates a purchase token and returns its claims.
// Expired tokens return ErrTokenExpired so callers can distinguish
// "window closed" (410) from "forged/wrong token" (401).
func ParsePurchaseToken(tokenString, secret string) (*PurchaseClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &PurchaseClaims{},
		func(t *jwt.Token) (any, error) { return []byte(secret), nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}
	claims, ok := token.Claims.(*PurchaseClaims)
	if !ok || !token.Valid {
		return nil, ErrTokenInvalid
	}
	return claims, nil
}

// ParseToken validates an HS256 JWT and returns its claims.
func ParseToken(tokenString, secret string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{},
		func(t *jwt.Token) (any, error) { return []byte(secret), nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, ErrTokenInvalid
	}
	return claims, nil
}
