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

// Package conformance is a provider-agnostic test suite for
// provisioner.Provisioner implementations. Every correctness/safety
// property the interface promises (idempotency, the ownership-marker
// safety net, rotation semantics, partial-failure resumability) is
// expressed here once, against the interface, rather than reimplemented
// per provider. A new Provisioner (Aurora, Cloud SQL, ...) gets this whole
// battery for free by implementing Inspector and calling Run from its own
// test suite; see internal/provisioner/postgres/postgres_test.go for the
// reference wiring.
package conformance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // dot-import matches this repo's existing Ginkgo test convention
	. "github.com/onsi/gomega"    //nolint:staticcheck

	"github.com/bpalko/sprout/internal/provisioner"
)

// Inspector is the only provider-specific seam the conformance suite
// needs: read-only ways to verify real state that aren't exposed through
// Provisioner itself. It must observe the same target instance the
// Harness's Provisioner operates against.
type Inspector interface {
	DatabaseExists(ctx context.Context, name string) (bool, error)
	// DatabaseOwnerMarker returns the ownership-marker comment stamped on
	// the database, and whether one exists at all.
	DatabaseOwnerMarker(ctx context.Context, name string) (marker string, exists bool, err error)
	// DatabaseOwnerRole returns the database's current owning role.
	DatabaseOwnerRole(ctx context.Context, name string) (string, error)

	RoleExists(ctx context.Context, name string) (bool, error)
	RoleOwnerMarker(ctx context.Context, name string) (marker string, exists bool, err error)
	RoleConnectionLimit(ctx context.Context, name string) (int32, error)

	// CanAuthenticate attempts a real login as roleName/password against
	// database, returning whether it succeeded. Used to prove rotation
	// actually invalidates old credentials and activates new ones, not
	// just that EnsureRole returned no error.
	CanAuthenticate(ctx context.Context, roleName, password, database string) (bool, error)
}

// Harness wires a Provisioner under test to the target instance it
// operates against and an Inspector able to verify that instance's real
// state.
//
// Conn and Inspector are resolved lazily (called from inside each spec's
// body, not while the spec tree is being built) because Run is meant to
// be called from a top-level `var _ = Describe(...)`, and like every
// Ginkgo tree-construction closure, that runs at package init, before
// BeforeSuite has had a chance to start the target instance (e.g. spin up
// a testcontainers container) and learn its address. Provisioner doesn't
// need this treatment: every provisioner.Provisioner method already takes
// a ConnectionConfig as an explicit argument, so the value itself can be
// constructed up front.
type Harness struct {
	Provisioner provisioner.Provisioner
	Conn        func() provisioner.ConnectionConfig
	Inspector   func() Inspector
}

// Run registers the full conformance spec tree against h. Call it from
// inside a provider's own Ginkgo suite during spec-tree construction
// (i.e. from a top-level `var _ = Describe(...)` or similar); h.Conn and
// h.Inspector are only invoked once specs actually run, by which point
// the target instance must be reachable (e.g. started in BeforeSuite).
func Run(h Harness) {
	Describe("Provisioner conformance", func() {
		ctx := context.Background()

		Describe("EnsureDatabase", func() {
			It("creates a missing database and stamps its ownership marker", func() {
				db, owner := uniqueName("db"), uniqueOwner()
				spec := provisioner.DatabaseSpec{DatabaseName: db, OwnerID: owner}

				Expect(h.Provisioner.EnsureDatabase(ctx, h.Conn(), spec)).To(Succeed())

				exists, err := h.Inspector().DatabaseExists(ctx, db)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue())

				marker, markerExists, err := h.Inspector().DatabaseOwnerMarker(ctx, db)
				Expect(err).NotTo(HaveOccurred())
				Expect(markerExists).To(BeTrue())
				Expect(marker).To(ContainSubstring(owner))
			})

			It("is idempotent when called again with the same owner", func() {
				db, owner := uniqueName("db"), uniqueOwner()
				spec := provisioner.DatabaseSpec{DatabaseName: db, OwnerID: owner}

				Expect(h.Provisioner.EnsureDatabase(ctx, h.Conn(), spec)).To(Succeed())
				Expect(h.Provisioner.EnsureDatabase(ctx, h.Conn(), spec)).To(Succeed())

				exists, err := h.Inspector().DatabaseExists(ctx, db)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue())
			})

			It("refuses to adopt a database owned by a different OwnerID", func() {
				db, ownerA, ownerB := uniqueName("db"), uniqueOwner(), uniqueOwner()
				Expect(h.Provisioner.EnsureDatabase(ctx, h.Conn(), provisioner.DatabaseSpec{DatabaseName: db, OwnerID: ownerA})).To(Succeed())

				err := h.Provisioner.EnsureDatabase(ctx, h.Conn(), provisioner.DatabaseSpec{DatabaseName: db, OwnerID: ownerB})
				Expect(err).To(HaveOccurred())

				marker, markerExists, ierr := h.Inspector().DatabaseOwnerMarker(ctx, db)
				Expect(ierr).NotTo(HaveOccurred())
				Expect(markerExists).To(BeTrue())
				Expect(marker).To(ContainSubstring(ownerA), "database must still be owned by the original OwnerID, not adopted or overwritten")
			})
		})

		Describe("EnsureRole", func() {
			It("creates a missing role, applies the connection limit, and makes it the database owner", func() {
				db, role, owner := uniqueName("db"), uniqueName("role"), uniqueOwner()
				Expect(h.Provisioner.EnsureDatabase(ctx, h.Conn(), provisioner.DatabaseSpec{DatabaseName: db, OwnerID: owner})).To(Succeed())

				creds, err := h.Provisioner.EnsureRole(ctx, h.Conn(), provisioner.RoleSpec{
					DatabaseName: db, RoleName: role, OwnerID: owner, ConnectionLimit: 7,
				}, true)
				Expect(err).NotTo(HaveOccurred())
				Expect(creds.Username).To(Equal(role))
				Expect(creds.Password).NotTo(BeEmpty())

				limit, err := h.Inspector().RoleConnectionLimit(ctx, role)
				Expect(err).NotTo(HaveOccurred())
				Expect(limit).To(Equal(int32(7)))

				ownerRole, err := h.Inspector().DatabaseOwnerRole(ctx, db)
				Expect(err).NotTo(HaveOccurred())
				Expect(ownerRole).To(Equal(role), "role must become the database owner (grants public-schema CREATE via pg_database_owner)")

				marker, markerExists, err := h.Inspector().RoleOwnerMarker(ctx, role)
				Expect(err).NotTo(HaveOccurred())
				Expect(markerExists).To(BeTrue())
				Expect(marker).To(ContainSubstring(owner))

				By("actually authenticating and writing data as the returned credentials")
				ok, err := h.Inspector().CanAuthenticate(ctx, role, creds.Password, db)
				Expect(err).NotTo(HaveOccurred())
				Expect(ok).To(BeTrue())
			})

			It("reconciles the connection limit without rotating an untouched password", func() {
				db, role, owner := uniqueName("db"), uniqueName("role"), uniqueOwner()
				Expect(h.Provisioner.EnsureDatabase(ctx, h.Conn(), provisioner.DatabaseSpec{DatabaseName: db, OwnerID: owner})).To(Succeed())
				created, err := h.Provisioner.EnsureRole(ctx, h.Conn(), provisioner.RoleSpec{
					DatabaseName: db, RoleName: role, OwnerID: owner, ConnectionLimit: 5,
				}, true)
				Expect(err).NotTo(HaveOccurred())

				_, err = h.Provisioner.EnsureRole(ctx, h.Conn(), provisioner.RoleSpec{
					DatabaseName: db, RoleName: role, OwnerID: owner, ConnectionLimit: 9,
				}, false)
				Expect(err).NotTo(HaveOccurred())

				limit, err := h.Inspector().RoleConnectionLimit(ctx, role)
				Expect(err).NotTo(HaveOccurred())
				Expect(limit).To(Equal(int32(9)))

				ok, err := h.Inspector().CanAuthenticate(ctx, role, created.Password, db)
				Expect(err).NotTo(HaveOccurred())
				Expect(ok).To(BeTrue(), "original password must still authenticate when rotate=false")
			})

			It("rotates the password when rotate=true, invalidating the old one", func() {
				db, role, owner := uniqueName("db"), uniqueName("role"), uniqueOwner()
				Expect(h.Provisioner.EnsureDatabase(ctx, h.Conn(), provisioner.DatabaseSpec{DatabaseName: db, OwnerID: owner})).To(Succeed())
				created, err := h.Provisioner.EnsureRole(ctx, h.Conn(), provisioner.RoleSpec{
					DatabaseName: db, RoleName: role, OwnerID: owner, ConnectionLimit: 5,
				}, true)
				Expect(err).NotTo(HaveOccurred())

				rotated, err := h.Provisioner.EnsureRole(ctx, h.Conn(), provisioner.RoleSpec{
					DatabaseName: db, RoleName: role, OwnerID: owner, ConnectionLimit: 5,
				}, true)
				Expect(err).NotTo(HaveOccurred())
				Expect(rotated.Password).NotTo(Equal(created.Password))

				oldOK, err := h.Inspector().CanAuthenticate(ctx, role, created.Password, db)
				Expect(err).NotTo(HaveOccurred())
				Expect(oldOK).To(BeFalse(), "old password must stop working after rotation")

				newOK, err := h.Inspector().CanAuthenticate(ctx, role, rotated.Password, db)
				Expect(err).NotTo(HaveOccurred())
				Expect(newOK).To(BeTrue())
			})

			It("refuses to mutate a role owned by a different OwnerID", func() {
				db, role, ownerA, ownerB := uniqueName("db"), uniqueName("role"), uniqueOwner(), uniqueOwner()
				Expect(h.Provisioner.EnsureDatabase(ctx, h.Conn(), provisioner.DatabaseSpec{DatabaseName: db, OwnerID: ownerA})).To(Succeed())
				created, err := h.Provisioner.EnsureRole(ctx, h.Conn(), provisioner.RoleSpec{
					DatabaseName: db, RoleName: role, OwnerID: ownerA, ConnectionLimit: 5,
				}, true)
				Expect(err).NotTo(HaveOccurred())

				_, err = h.Provisioner.EnsureRole(ctx, h.Conn(), provisioner.RoleSpec{
					DatabaseName: db, RoleName: role, OwnerID: ownerB, ConnectionLimit: 99,
				}, true)
				Expect(err).To(HaveOccurred())

				limit, err := h.Inspector().RoleConnectionLimit(ctx, role)
				Expect(err).NotTo(HaveOccurred())
				Expect(limit).To(Equal(int32(5)), "connection limit must not change on an ownership-mismatched call")

				ok, err := h.Inspector().CanAuthenticate(ctx, role, created.Password, db)
				Expect(err).NotTo(HaveOccurred())
				Expect(ok).To(BeTrue(), "password must not change on an ownership-mismatched call")
			})
		})

		Describe("Teardown", func() {
			It("drops a database and role it owns", func() {
				db, role, owner := uniqueName("db"), uniqueName("role"), uniqueOwner()
				Expect(h.Provisioner.EnsureDatabase(ctx, h.Conn(), provisioner.DatabaseSpec{DatabaseName: db, OwnerID: owner})).To(Succeed())
				_, err := h.Provisioner.EnsureRole(ctx, h.Conn(), provisioner.RoleSpec{
					DatabaseName: db, RoleName: role, OwnerID: owner, ConnectionLimit: 5,
				}, true)
				Expect(err).NotTo(HaveOccurred())

				Expect(h.Provisioner.Teardown(ctx, h.Conn(), provisioner.DatabaseSpec{DatabaseName: db, OwnerID: owner}, role)).To(Succeed())

				dbExists, err := h.Inspector().DatabaseExists(ctx, db)
				Expect(err).NotTo(HaveOccurred())
				Expect(dbExists).To(BeFalse())
				roleExists, err := h.Inspector().RoleExists(ctx, role)
				Expect(err).NotTo(HaveOccurred())
				Expect(roleExists).To(BeFalse())
			})

			It("is a no-op when the database and role never existed", func() {
				db, role, owner := uniqueName("db"), uniqueName("role"), uniqueOwner()
				Expect(h.Provisioner.Teardown(ctx, h.Conn(), provisioner.DatabaseSpec{DatabaseName: db, OwnerID: owner}, role)).To(Succeed())
			})

			It("is idempotent when called twice", func() {
				db, role, owner := uniqueName("db"), uniqueName("role"), uniqueOwner()
				Expect(h.Provisioner.EnsureDatabase(ctx, h.Conn(), provisioner.DatabaseSpec{DatabaseName: db, OwnerID: owner})).To(Succeed())
				_, err := h.Provisioner.EnsureRole(ctx, h.Conn(), provisioner.RoleSpec{
					DatabaseName: db, RoleName: role, OwnerID: owner, ConnectionLimit: 5,
				}, true)
				Expect(err).NotTo(HaveOccurred())

				dbSpec := provisioner.DatabaseSpec{DatabaseName: db, OwnerID: owner}
				Expect(h.Provisioner.Teardown(ctx, h.Conn(), dbSpec, role)).To(Succeed())
				Expect(h.Provisioner.Teardown(ctx, h.Conn(), dbSpec, role)).To(Succeed())
			})

			It("refuses to drop a database or role owned by a different OwnerID, leaving both intact", func() {
				db, role, ownerA, ownerB := uniqueName("db"), uniqueName("role"), uniqueOwner(), uniqueOwner()
				Expect(h.Provisioner.EnsureDatabase(ctx, h.Conn(), provisioner.DatabaseSpec{DatabaseName: db, OwnerID: ownerA})).To(Succeed())
				created, err := h.Provisioner.EnsureRole(ctx, h.Conn(), provisioner.RoleSpec{
					DatabaseName: db, RoleName: role, OwnerID: ownerA, ConnectionLimit: 5,
				}, true)
				Expect(err).NotTo(HaveOccurred())

				err = h.Provisioner.Teardown(ctx, h.Conn(), provisioner.DatabaseSpec{DatabaseName: db, OwnerID: ownerB}, role)
				Expect(err).To(HaveOccurred(), "must refuse to tear down a database/role it doesn't own")

				dbExists, err := h.Inspector().DatabaseExists(ctx, db)
				Expect(err).NotTo(HaveOccurred())
				Expect(dbExists).To(BeTrue(), "database must survive an ownership-mismatched Teardown")
				roleExists, err := h.Inspector().RoleExists(ctx, role)
				Expect(err).NotTo(HaveOccurred())
				Expect(roleExists).To(BeTrue(), "role must survive an ownership-mismatched Teardown")
				ok, err := h.Inspector().CanAuthenticate(ctx, role, created.Password, db)
				Expect(err).NotTo(HaveOccurred())
				Expect(ok).To(BeTrue(), "credentials must remain valid after an ownership-mismatched Teardown")
			})
		})

		Describe("partial-failure resume", func() {
			It("reaches the same state whether EnsureDatabase/EnsureRole run together or the reconcile is retried between them", func() {
				db, role, owner := uniqueName("db"), uniqueName("role"), uniqueOwner()

				// First "reconcile pass": only gets as far as EnsureDatabase.
				Expect(h.Provisioner.EnsureDatabase(ctx, h.Conn(), provisioner.DatabaseSpec{DatabaseName: db, OwnerID: owner})).To(Succeed())

				// Retried reconcile: EnsureDatabase runs again (idempotent),
				// then EnsureRole proceeds. Nothing about the earlier
				// partial attempt should trip it up.
				Expect(h.Provisioner.EnsureDatabase(ctx, h.Conn(), provisioner.DatabaseSpec{DatabaseName: db, OwnerID: owner})).To(Succeed())
				creds, err := h.Provisioner.EnsureRole(ctx, h.Conn(), provisioner.RoleSpec{
					DatabaseName: db, RoleName: role, OwnerID: owner, ConnectionLimit: 5,
				}, true)
				Expect(err).NotTo(HaveOccurred())

				ok, err := h.Inspector().CanAuthenticate(ctx, role, creds.Password, db)
				Expect(err).NotTo(HaveOccurred())
				Expect(ok).To(BeTrue())
			})
		})
	})
}

func uniqueName(kind string) string {
	return fmt.Sprintf("sprout_conformance_%s_%s", kind, randomHex(4))
}

func uniqueOwner() string {
	return "owner-" + randomHex(8)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err) // crypto/rand failing is not something a test suite can meaningfully recover from
	}
	return hex.EncodeToString(buf)
}
