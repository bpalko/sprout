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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bpalko/sprout/internal/provisioner"
	"github.com/bpalko/sprout/internal/provisioner/conformance"
)

// TestPostgres runs the shared provisioner conformance suite
// (internal/provisioner/conformance) against a real Postgres instance
// started in a Docker container via testcontainers-go. This is the
// reference wiring a future Postgres-wire-compatible provider (Aurora,
// Cloud SQL) can copy: stand up a reachable instance in BeforeSuite, build
// a provisioner.ConnectionConfig and an Inspector for it, hand both
// (lazily, see conformance.Harness) to conformance.Run.
func TestPostgres(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "postgres Provisioner Conformance Suite")
}

// pgSuperuser is both the testcontainers-provisioned superuser's name and
// the database it connects to by default (the "postgres" database always
// exists on a fresh instance).
const pgSuperuser = "postgres"

// conn is populated by BeforeSuite once the container is up. The
// Describe below runs at package init, long before BeforeSuite, so
// conformance.Harness reads this through a closure rather than a value
// captured at construction time.
var conn provisioner.ConnectionConfig

var _ = BeforeSuite(func(ctx context.Context) {
	container, err := tcpostgres.Run(ctx, "postgres:16",
		tcpostgres.WithDatabase(pgSuperuser),
		tcpostgres.WithUsername(pgSuperuser),
		tcpostgres.WithPassword(pgSuperuser),
		tcpostgres.BasicWaitStrategies(),
	)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx context.Context) error {
		return container.Terminate(ctx)
	})

	host, err := container.Host(ctx)
	Expect(err).NotTo(HaveOccurred())
	port, err := container.MappedPort(ctx, "5432/tcp")
	Expect(err).NotTo(HaveOccurred())

	conn = provisioner.ConnectionConfig{
		Host:          host,
		Port:          int32(port.Num()),
		AdminUser:     pgSuperuser,
		AdminPassword: pgSuperuser,
		AdminDatabase: pgSuperuser,
	}
})

var _ = Describe("postgres.Provisioner", func() {
	conformance.Run(conformance.Harness{
		Provisioner: New(),
		Conn:        func() provisioner.ConnectionConfig { return conn },
		Inspector:   func() conformance.Inspector { return NewInspector(conn) },
	})
})
