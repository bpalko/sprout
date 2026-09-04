/*
Copyright 2026 bpalko.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package postgres implements provisioner.Provisioner against a Postgres
// (or wire-compatible) instance using pgx.
package postgres

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/bpalko/sprout/internal/provisioner"
)

// ownerMarker is the COMMENT text stamped on a database/role at creation and
// checked before any later mutation or drop, so a name collision with
// something provisioned out-of-band is never silently adopted. See
// DESIGN.md's "Ownership marker" section.
func ownerMarker(ownerID string) string {
	return "sprout-owner=" + ownerID
}

// Provisioner implements provisioner.Provisioner against Postgres.
type Provisioner struct{}

// New returns a Postgres Provisioner.
func New() *Provisioner {
	return &Provisioner{}
}

var _ provisioner.Provisioner = (*Provisioner)(nil)

// connect opens a short-lived admin connection. Sprout's call volume is low
// (reconciles, not request-path traffic), so a pooled connection isn't
// worth the added complexity (yet???), and every call just opens and closes its own.
func connect(ctx context.Context, conn provisioner.ConnectionConfig) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=prefer",
		conn.Host, conn.Port, conn.AdminUser, conn.AdminPassword, conn.AdminDatabase,
	))
	if err != nil {
		return nil, fmt.Errorf("parsing admin connection config: %w", err)
	}
	pgconn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s:%d as admin: %w", conn.Host, conn.Port, err)
	}
	return pgconn, nil
}

// checkOwnership returns nil if the shared-catalog comment on the object
// identified by catalogQuery matches ownerID, an error if it exists but
// doesn't match, and errNotFound if the object doesn't exist at all.
var errNotFound = errors.New("object not found")

func checkOwnership(ctx context.Context, pgconn *pgx.Conn, descriptionQuery, name, ownerID string) error {
	var description *string
	err := pgconn.QueryRow(ctx, descriptionQuery, name).Scan(&description)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return fmt.Errorf("checking ownership marker: %w", err)
	}
	want := ownerMarker(ownerID)
	if description == nil || *description != want {
		return fmt.Errorf("existing object %q is not owned by this Sprout (ownership marker mismatch)", name)
	}
	return nil
}

const databaseDescriptionQuery = `
SELECT sd.description
FROM pg_database d
LEFT JOIN pg_shdescription sd ON sd.objoid = d.oid
WHERE d.datname = $1`

const roleDescriptionQuery = `
SELECT sd.description
FROM pg_roles r
LEFT JOIN pg_shdescription sd ON sd.objoid = r.oid
WHERE r.rolname = $1`

// EnsureDatabase implements provisioner.Provisioner.
func (p *Provisioner) EnsureDatabase(ctx context.Context, conn provisioner.ConnectionConfig, spec provisioner.DatabaseSpec) error {
	pgconn, err := connect(ctx, conn)
	if err != nil {
		return err
	}
	defer func() { _ = pgconn.Close(ctx) }()

	quoted := pgx.Identifier{spec.DatabaseName}.Sanitize()

	err = checkOwnership(ctx, pgconn, databaseDescriptionQuery, spec.DatabaseName, spec.OwnerID)
	switch {
	case errors.Is(err, errNotFound):
		// fall through to creation below
	case err != nil:
		return err
	default:
		return nil // exists and owned by this Sprout; nothing to do
	}

	if _, err := pgconn.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		return fmt.Errorf("creating database %q: %w", spec.DatabaseName, err)
	}
	if err := commentOn(ctx, pgconn, "DATABASE", quoted, ownerMarker(spec.OwnerID)); err != nil {
		return fmt.Errorf("stamping ownership marker on database %q: %w", spec.DatabaseName, err)
	}
	return nil
}

// commentOn issues COMMENT ON <kind> <quotedName> IS '<text>', escaping text
// as a SQL string literal (COMMENT ON doesn't accept a bind parameter for
// its text in all drivers' statement forms, so this quotes it directly).
func commentOn(ctx context.Context, pgconn *pgx.Conn, kind, quotedName, text string) error {
	literal := "'" + stringEscape(text) + "'"
	_, err := pgconn.Exec(ctx, fmt.Sprintf("COMMENT ON %s %s IS %s", kind, quotedName, literal))
	return err
}

func stringEscape(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'')
		}
		out = append(out, s[i])
	}
	return string(out)
}

// generatePassword returns a random 32-byte, URL-safe-base64-encoded
// password suitable for a Postgres role.
func generatePassword() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// EnsureRole implements provisioner.Provisioner.
func (p *Provisioner) EnsureRole(ctx context.Context, conn provisioner.ConnectionConfig, spec provisioner.RoleSpec, rotate bool) (provisioner.Credentials, error) {
	pgconn, err := connect(ctx, conn)
	if err != nil {
		return provisioner.Credentials{}, err
	}
	defer func() { _ = pgconn.Close(ctx) }()

	quotedRole := pgx.Identifier{spec.RoleName}.Sanitize()
	quotedDB := pgx.Identifier{spec.DatabaseName}.Sanitize()

	err = checkOwnership(ctx, pgconn, roleDescriptionQuery, spec.RoleName, spec.OwnerID)
	created := errors.Is(err, errNotFound)
	if err != nil && !created {
		return provisioner.Credentials{}, err
	}

	password := ""
	if created || rotate {
		password, err = generatePassword()
		if err != nil {
			return provisioner.Credentials{}, err
		}
	}

	if created {
		literal := "'" + stringEscape(password) + "'"
		stmt := fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD %s CONNECTION LIMIT %d", quotedRole, literal, spec.ConnectionLimit)
		if _, err := pgconn.Exec(ctx, stmt); err != nil {
			return provisioner.Credentials{}, fmt.Errorf("creating role %q: %w", spec.RoleName, err)
		}
		if err := commentOn(ctx, pgconn, "ROLE", quotedRole, ownerMarker(spec.OwnerID)); err != nil {
			return provisioner.Credentials{}, fmt.Errorf("stamping ownership marker on role %q: %w", spec.RoleName, err)
		}
	} else {
		stmt := fmt.Sprintf("ALTER ROLE %s CONNECTION LIMIT %d", quotedRole, spec.ConnectionLimit)
		if _, err := pgconn.Exec(ctx, stmt); err != nil {
			return provisioner.Credentials{}, fmt.Errorf("reconciling connection limit on role %q: %w", spec.RoleName, err)
		}
		if rotate {
			literal := "'" + stringEscape(password) + "'"
			if _, err := pgconn.Exec(ctx, fmt.Sprintf("ALTER ROLE %s PASSWORD %s", quotedRole, literal)); err != nil {
				return provisioner.Credentials{}, fmt.Errorf("rotating password on role %q: %w", spec.RoleName, err)
			}
		}
	}

	// Ownership makes the role a member of Postgres 15+'s implicit
	// pg_database_owner for this database, which is what grants it CREATE
	// on the public schema, so no separate schema-level GRANT is needed.
	if _, err := pgconn.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", quotedDB, quotedRole)); err != nil {
		return provisioner.Credentials{}, fmt.Errorf("setting database %q owner to role %q: %w", spec.DatabaseName, spec.RoleName, err)
	}

	if password == "" {
		// Existing role, no rotation requested: the caller already has the
		// current credentials (from the Secret) and isn't expected to use
		// this return value.
		return provisioner.Credentials{Username: spec.RoleName}, nil
	}
	return provisioner.Credentials{Username: spec.RoleName, Password: password}, nil
}

// Teardown implements provisioner.Provisioner.
func (p *Provisioner) Teardown(ctx context.Context, conn provisioner.ConnectionConfig, spec provisioner.DatabaseSpec, roleName string) error {
	pgconn, err := connect(ctx, conn)
	if err != nil {
		return err
	}
	defer func() { _ = pgconn.Close(ctx) }()

	quotedDB := pgx.Identifier{spec.DatabaseName}.Sanitize()
	err = checkOwnership(ctx, pgconn, databaseDescriptionQuery, spec.DatabaseName, spec.OwnerID)
	switch {
	case errors.Is(err, errNotFound):
		// already gone; nothing to drop
	case err != nil:
		return err
	default:
		if _, err := pgconn.Exec(ctx, "DROP DATABASE "+quotedDB+" WITH (FORCE)"); err != nil {
			return fmt.Errorf("dropping database %q: %w", spec.DatabaseName, err)
		}
	}

	quotedRole := pgx.Identifier{roleName}.Sanitize()
	err = checkOwnership(ctx, pgconn, roleDescriptionQuery, roleName, spec.OwnerID)
	switch {
	case errors.Is(err, errNotFound):
		return nil // already gone
	case err != nil:
		return err
	default:
		if _, err := pgconn.Exec(ctx, "DROP ROLE "+quotedRole); err != nil {
			return fmt.Errorf("dropping role %q: %w", roleName, err)
		}
		return nil
	}
}
