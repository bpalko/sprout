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

// Package provisioner defines the boundary between the Sprout controller
// and a target database instance. Everything the controller knows about
// "postgres" vs "aurora" vs "cloudsql" lives behind this interface. The
// reconciler itself only ever talks to a Provisioner.
package provisioner

import "context"

// ConnectionConfig describes how to reach a target database instance as an
// admin. It is built from a Sprout's spec.connection plus the resolved
// contents of its adminSecretRef.
type ConnectionConfig struct {
	Host          string
	Port          int32
	AdminUser     string
	AdminPassword string
	// AdminDatabase is the database the admin connection is made against
	// to issue CREATE DATABASE/ROLE statements (e.g. "postgres").
	AdminDatabase string
}

// DatabaseSpec identifies the logical database a Provisioner operates on,
// and the marker used to prove ownership before any mutating or
// destructive call.
type DatabaseSpec struct {
	// DatabaseName is the logical database name to ensure exists.
	DatabaseName string
	// OwnerID uniquely identifies the owning Sprout (its UID). Stamped on
	// the database at creation and checked before any later mutation or
	// drop, so the provisioner never touches a database it didn't create
	// (say, on a name collision with something provisioned out-of-band).
	OwnerID string
}

// RoleSpec identifies the login role a Provisioner ensures exists for a
// tenant database, and the limits applied to it.
type RoleSpec struct {
	DatabaseName string
	RoleName     string
	OwnerID      string
	// ConnectionLimit caps concurrent connections held by this role. A
	// negative value means unlimited.
	ConnectionLimit int32
}

// Credentials are the login credentials for a tenant's role, handed back
// to the controller to publish into a Secret.
type Credentials struct {
	Username string
	Password string
}

// Provisioner manages logical databases, roles, and credentials on a
// target database instance for one Sprout at a time.
//
// Every method must be idempotent and safe to call repeatedly with the
// same arguments: Ensure* methods create-if-missing and otherwise leave
// existing, correctly-owned state alone, so a reconcile that fails partway
// through (e.g. database created, role creation fails) can simply be
// retried from the top without special-case recovery logic.
type Provisioner interface {
	// EnsureDatabase creates DatabaseSpec.DatabaseName if it doesn't
	// exist, stamping it with an ownership marker derived from OwnerID.
	// If a database with that name already exists but its ownership
	// marker doesn't match OwnerID, EnsureDatabase returns an error
	// rather than adopting it.
	EnsureDatabase(ctx context.Context, conn ConnectionConfig, spec DatabaseSpec) error

	// EnsureRole creates RoleSpec.RoleName if it doesn't exist (owned by
	// and scoped to RoleSpec.DatabaseName), applies ConnectionLimit, and
	// returns its current credentials. When rotate is true, or the role
	// was just created, a new random password is generated and applied.
	// Ownership is verified the same way as EnsureDatabase before any
	// mutation.
	EnsureRole(ctx context.Context, conn ConnectionConfig, spec RoleSpec, rotate bool) (Credentials, error)

	// Teardown drops the role and database identified by spec, after
	// verifying both are owned by spec.OwnerID. It is safe to call when
	// the role and/or database are already gone.
	Teardown(ctx context.Context, conn ConnectionConfig, spec DatabaseSpec, roleName string) error
}
