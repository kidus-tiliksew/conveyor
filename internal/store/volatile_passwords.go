package store

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// These Argon2id parameters deliberately favor memory over parallelism: three
// passes over 64 MiB make offline guessing expensive without letting one web
// request consume an unreasonable share of a small self-hosted deployment.
const (
	passwordArgonMemory  uint32 = 64 * 1024
	passwordArgonTime    uint32 = 3
	passwordArgonThreads uint8  = 2
	passwordSaltBytes           = 16
	passwordHashBytes    uint32 = 32
)

var (
	dummyPasswordHashOnce sync.Once
	dummyPasswordHash     string
)

// volatileDummyPasswordHash lazily builds the fixed, valid hash used for unknown
// and passwordless accounts. Deferring the intentionally expensive Argon2id
// work avoids imposing it on unrelated process startup. The result is not a
// credential and is never persisted.
func volatileDummyPasswordHash() string {
	dummyPasswordHashOnce.Do(func() {
		dummyPasswordHash = encodePasswordHashWithSalt("conveyor-invalid-password", []byte("cv-dummy-salt-v1"))
	})
	return dummyPasswordHash
}

func hashVolatilePassword(password string) (string, error) {
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	return encodePasswordHashWithSalt(password, salt), nil
}

func encodePasswordHashWithSalt(password string, salt []byte) string {
	digest := argon2.IDKey([]byte(password), salt, passwordArgonTime, passwordArgonMemory, passwordArgonThreads, passwordHashBytes)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, passwordArgonMemory, passwordArgonTime, passwordArgonThreads, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(digest))
}

func verifyVolatilePassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return false
	}
	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil || memory != passwordArgonMemory || iterations != passwordArgonTime || threads != passwordArgonThreads {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != passwordSaltBytes {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) != int(passwordHashBytes) {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func validVolatilePassword(password string) bool {
	return len(password) >= 12 && len(password) <= 1024
}
