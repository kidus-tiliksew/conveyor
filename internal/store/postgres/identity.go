package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

var ErrInvalidPersonalAccessToken = errors.New("invalid personal access token")

type IdentityUser struct {
	ID          string
	Email       string
	DisplayName string
	Status      string
	CreatedAt   time.Time
}

type PersonalAccessToken struct {
	ID         string
	UserID     string
	Label      string
	LastUsedAt *time.Time
	RevokedAt  *time.Time
	CreatedAt  time.Time
}

type IssuedPersonalAccessToken struct {
	PersonalAccessToken
	Value string
}

// BootstrapIdentity seeds the singleton organization, first operator, and the
// legacy shared bearer token exactly once. The transaction lock makes parallel
// daemon starts converge without replacing any existing identity row.
func (s *Store) BootstrapIdentity(ctx context.Context, identity config.FirstOperatorIdentity, legacyToken string) (bool, error) {
	identity.Email = strings.ToLower(strings.TrimSpace(identity.Email))
	identity.DisplayName = strings.TrimSpace(identity.DisplayName)
	identity.OrganizationName = strings.TrimSpace(identity.OrganizationName)
	address, err := mail.ParseAddress(identity.Email)
	if err != nil || address.Address != identity.Email || !strings.Contains(identity.Email, "@") {
		return false, errors.New("first operator email must be a valid email address")
	}
	if identity.DisplayName == "" || identity.OrganizationName == "" {
		return false, errors.New("organization name and first operator display name are required")
	}
	if legacyToken == "" {
		return false, errors.New("legacy API token is required for identity bootstrap")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('conveyor:identity-bootstrap'))"); err != nil {
		return false, fmt.Errorf("lock identity bootstrap: %w", err)
	}
	q := s.queries.WithTx(tx)
	count, err := q.CountUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("count identity users: %w", err)
	}
	if count != 0 {
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return false, nil
	}
	if _, err := q.UpdateDeploymentOrgName(ctx, identity.OrganizationName); err != nil {
		return false, fmt.Errorf("configure deployment organization: %w", err)
	}
	userID, err := randomIdentityID("usr", 12)
	if err != nil {
		return false, err
	}
	user, err := q.InsertIdentityUser(ctx, db.InsertIdentityUserParams{ID: userID, Email: identity.Email, DisplayName: identity.DisplayName})
	if err != nil {
		return false, fmt.Errorf("seed first operator: %w", err)
	}
	tokenID, err := randomIdentityID("pat", 12)
	if err != nil {
		return false, err
	}
	hash := sha256.Sum256([]byte(legacyToken))
	if _, err := q.InsertUserToken(ctx, db.InsertUserTokenParams{ID: tokenID, UserID: user.ID, Label: "legacy API token", TokenHash: hash[:]}); err != nil {
		return false, fmt.Errorf("map legacy API token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit identity bootstrap: %w", err)
	}
	return true, nil
}

func (s *Store) IssuePersonalAccessToken(ctx context.Context, userID, label string) (IssuedPersonalAccessToken, error) {
	user, err := s.queries.GetIdentityUser(ctx, userID)
	if err != nil {
		return IssuedPersonalAccessToken{}, notFound(err, "identity user %s", userID)
	}
	if user.Status != "active" {
		return IssuedPersonalAccessToken{}, errors.New("cannot issue a token for a deactivated user")
	}
	tokenID, err := randomIdentityID("pat", 12)
	if err != nil {
		return IssuedPersonalAccessToken{}, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return IssuedPersonalAccessToken{}, fmt.Errorf("generate personal access token: %w", err)
	}
	value := "cv_pat_" + tokenID + "_" + base64.RawURLEncoding.EncodeToString(secret)
	hash := sha256.Sum256([]byte(value))
	row, err := s.queries.InsertUserToken(ctx, db.InsertUserTokenParams{ID: tokenID, UserID: userID, Label: strings.TrimSpace(label), TokenHash: hash[:]})
	if err != nil {
		return IssuedPersonalAccessToken{}, err
	}
	return IssuedPersonalAccessToken{PersonalAccessToken: personalAccessToken(row), Value: value}, nil
}

func (s *Store) VerifyPersonalAccessToken(ctx context.Context, candidate string) (IdentityUser, error) {
	hash := sha256.Sum256([]byte(candidate))
	row, err := s.queries.GetUserTokenByHash(ctx, hash[:])
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return IdentityUser{}, ErrInvalidPersonalAccessToken
		}
		return IdentityUser{}, err
	}
	if subtle.ConstantTimeCompare(row.TokenHash, hash[:]) != 1 || row.RevokedAt.Valid || row.Status != "active" {
		return IdentityUser{}, ErrInvalidPersonalAccessToken
	}
	if err := s.queries.MarkUserTokenUsed(ctx, row.ID); err != nil {
		return IdentityUser{}, err
	}
	return IdentityUser{ID: row.UserID, Email: row.Email, DisplayName: row.DisplayName, Status: row.Status}, nil
}

func (s *Store) RevokePersonalAccessToken(ctx context.Context, tokenID string) (PersonalAccessToken, error) {
	row, err := s.queries.RevokeUserToken(ctx, tokenID)
	if err != nil {
		return PersonalAccessToken{}, notFound(err, "personal access token %s", tokenID)
	}
	return personalAccessToken(row), nil
}

func (s *Store) DeactivateIdentityUser(ctx context.Context, userID string) (IdentityUser, error) {
	row, err := s.queries.DeactivateIdentityUser(ctx, userID)
	if err != nil {
		return IdentityUser{}, notFound(err, "identity user %s", userID)
	}
	return identityUser(row), nil
}

func randomIdentityID(prefix string, size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate %s identity: %w", prefix, err)
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func identityUser(row db.User) IdentityUser {
	return IdentityUser{ID: row.ID, Email: row.Email, DisplayName: row.DisplayName, Status: row.Status, CreatedAt: row.CreatedAt.Time}
}

func personalAccessToken(row db.UserToken) PersonalAccessToken {
	item := PersonalAccessToken{ID: row.ID, UserID: row.UserID, Label: row.Label, CreatedAt: row.CreatedAt.Time}
	if row.LastUsedAt.Valid {
		value := row.LastUsedAt.Time
		item.LastUsedAt = &value
	}
	if row.RevokedAt.Valid {
		value := row.RevokedAt.Time
		item.RevokedAt = &value
	}
	return item
}
