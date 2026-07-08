// Package auth provides password hashing, JWT issuance/validation, and
// the HTTP middleware that guards authenticated routes.
package auth

import "github.com/alexedwards/argon2id"

// HashPassword hashes a plaintext password with argon2id.
func HashPassword(password string) (string, error) {
	return argon2id.CreateHash(password, argon2id.DefaultParams)
}

// VerifyPassword reports whether password matches the argon2id hash.
func VerifyPassword(password, hash string) (bool, error) {
	return argon2id.ComparePasswordAndHash(password, hash)
}
