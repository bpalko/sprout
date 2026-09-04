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

package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bpalko/sprout/internal/provisioner"
)

// Inspector provides read-only, Postgres-specific verification of state a
// Provisioner has produced. It doesn't itself implement any
// provisioner.Provisioner method. It's exported (not test-only) so a
// future Postgres-wire-compatible provider (Aurora, Cloud SQL both still
// speak the Postgres wire protocol per DESIGN.md's provider abstraction)
// can reuse it directly in its own tests instead of reimplementing these
// queries. It satisfies internal/provisioner/conformance.Inspector
// structurally; nothing here imports that package, so ginkgo/gomega never
// become a dependency of the production build.
type Inspector struct {
	admin provisioner.ConnectionConfig
}

// NewInspector returns an Inspector that queries the instance reachable
// via admin.
func NewInspector(admin provisioner.ConnectionConfig) *Inspector {
	return &Inspector{admin: admin}
}

func (i *Inspector) DatabaseExists(ctx context.Context, name string) (bool, error) {
	conn, err := connect(ctx, i.admin)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close(ctx) }()

	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		return false, fmt.Errorf("checking database %q exists: %w", name, err)
	}
	return exists, nil
}

func (i *Inspector) DatabaseOwnerMarker(ctx context.Context, name string) (string, bool, error) {
	return i.ownerMarker(ctx, databaseDescriptionQuery, name)
}

func (i *Inspector) DatabaseOwnerRole(ctx context.Context, name string) (string, error) {
	conn, err := connect(ctx, i.admin)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close(ctx) }()

	var owner string
	if err := conn.QueryRow(ctx, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = $1", name).Scan(&owner); err != nil {
		return "", fmt.Errorf("looking up owner of database %q: %w", name, err)
	}
	return owner, nil
}

func (i *Inspector) RoleExists(ctx context.Context, name string) (bool, error) {
	conn, err := connect(ctx, i.admin)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close(ctx) }()

	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = $1)", name).Scan(&exists); err != nil {
		return false, fmt.Errorf("checking role %q exists: %w", name, err)
	}
	return exists, nil
}

func (i *Inspector) RoleOwnerMarker(ctx context.Context, name string) (string, bool, error) {
	return i.ownerMarker(ctx, roleDescriptionQuery, name)
}

func (i *Inspector) RoleConnectionLimit(ctx context.Context, name string) (int32, error) {
	conn, err := connect(ctx, i.admin)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close(ctx) }()

	var limit int32
	if err := conn.QueryRow(ctx, "SELECT rolconnlimit FROM pg_roles WHERE rolname = $1", name).Scan(&limit); err != nil {
		return 0, fmt.Errorf("looking up connection limit of role %q: %w", name, err)
	}
	return limit, nil
}

func (i *Inspector) CanAuthenticate(ctx context.Context, roleName, password, database string) (bool, error) {
	cfg, err := pgx.ParseConfig(fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=prefer connect_timeout=5",
		i.admin.Host, i.admin.Port, roleName, password, database,
	))
	if err != nil {
		return false, fmt.Errorf("parsing connection config for role %q: %w", roleName, err)
	}

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && len(pgErr.Code) >= 2 && pgErr.Code[:2] == "28" {
			// Class 28 is Invalid Authorization Specification: wrong
			// password, unknown role, etc. This is the expected shape of
			// "authentication failed," not an infrastructure error.
			return false, nil
		}
		return false, fmt.Errorf("connecting as role %q: %w", roleName, err)
	}
	defer func() { _ = conn.Close(ctx) }()
	return true, nil
}

func (i *Inspector) ownerMarker(ctx context.Context, descriptionQuery, name string) (string, bool, error) {
	conn, err := connect(ctx, i.admin)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = conn.Close(ctx) }()

	var description *string
	err = conn.QueryRow(ctx, descriptionQuery, name).Scan(&description)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading ownership marker for %q: %w", name, err)
	}
	if description == nil {
		return "", false, nil
	}
	return *description, true, nil
}
