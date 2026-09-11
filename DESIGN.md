# Sprout: Design

> This is a living doc, not a point-in-time proposal. When a design
> decision changes, update this file in the same commit as the code
> change that follows from it. If this doc and the code ever disagree,
> that's a bug in one of them.

> I'm using it as a backboard for agentic dev, FYI.
> `AGENTS.md` holds more generic agent boundaries.

## What this is

Sprout is a Kubernetes operator that gives every preview environment (a
PR, a branch, a test run) its own isolated **logical database** on an
existing instance, created fresh with an empty schema and torn down
automatically when the owning `Sprout` object goes away.

You've already got a stable Postgres (or Aurora, or Cloud SQL; see
[Provider abstraction](#provider-abstraction)) sitting somewhere.
Rather than sharing one staging database across every open PR or
spinning up a whole new managed cluster per PR (slow, and not cheap
for a quick PR), Sprout serves to gen a new database, role, and set
of credentials inside the instance you already have, publishes
them, and lets your own migration tooling run against a clean
slate. Migrations and app tooling to me seem like a line in the
sand here. I just want to stand up the thing and let the user
decide what's next.

## What this deliberately is not

Sprout doesn't do data branching or cloning. A freshly-provisioned
database is empty (no data, no schema) until your migration tooling
touches it. That's a smaller, more portable idea than branching: logical
multi-tenancy works across any Postgres-wire-compatible backend, whereas
storage-level cloning is more provider-specific. If you need
production-shaped data in a preview environment, that's a seeding step
you own on top of what Sprout hands you, not something Sprout does
itself. I may consider template DBs but for now, throwing an empty
DB back to the user will suffice. Migrations can throw that seed data
in for a PR.

Sprout also doesn't manage the underlying instance's lifecycle. It
assumes one already exists, with admin credentials Sprout can use.
There's a million ways to provision and manage database clusters.
I'm not trying to redo that. I just want to stretch the bounds of
something already existing and make the ephemeral experience more fluid.

## Architecture

```
 ┌─────────────┐   watch/reconcile    ┌───────────────────┐
 │  Sprout CR   │◄─────────────────────│  Sprout controller │
 │ (namespaced) │                       └─────────┬──────────┘
 └─────────────┘                                 │
                                                   │ Provisioner interface
                                                   ▼
                                         ┌───────────────────┐
                                         │ postgres provider  │  (only one
                                         │ (implements the    │   implemented
                                         │  Provisioner iface) │   so far)
                                         └─────────┬──────────┘
                                                   │ CREATE DATABASE / ROLE
                                                   ▼
                                         ┌───────────────────┐
                                         │  target instance    │
                                         │  (in-cluster PG,     │
                                         │   Aurora, CloudSQL…) │
                                         └───────────────────┘

 controller also creates/rotates ──► Secret (same namespace as the Sprout)
                                       consumed by the app Deployment
```

One CRD, no separate "connection" resource: a `Sprout` carries its own
connection details inline. On complete object to represent some citizen
on some database. You can have many if you have many databases
(app database, analytics database, etc.)

## API: The `Sprout`

```yaml
apiVersion: db.sproutdb.dev/v1alpha1
kind: Sprout
metadata:
  name: myapp-pr-123
spec:
  provider: postgres                # immutable (CEL) - which Provisioner handles this
  connection:
    host: postgres.default.svc.cluster.local
    port: 5432
    adminSecretRef:
      name: postgres-admin-creds    # Secret in the same namespace; keys: username, password
    adminDatabase: postgres         # optional - defaults to "postgres"
    sslMode: prefer                 # optional - defaults to prefer
  databaseName: myapp_pr_123        # optional, immutable (CEL) - defaults to a derived name
  connectionLimit: 20               # optional, mutable - defaults to 20
status:
  phase: Ready                      # Pending | Provisioning | Ready | Terminating | Failed
  conditions: [...]                 # Ready, DatabaseProvisioned, SecretReady
  databaseName: myapp_pr_123
  roleName: myapp_pr_123_role
  secretName: myapp-pr-123
  observedGeneration: 3
```

Immutability on `provider` and `databaseName` is enforced with CEL
validation (`+kubebuilder:validation:XValidation`) right in the CRD
schema, so a bad `kubectl apply` gets rejected synchronously by the API
server itself. No controller round-trip needed. Should be clear as day
to the user why this action is bad and blocked.

## Reconcile loop

The thing this loop has to get right that a typical CRUD operator
doesn't is that the true state of what we're managing (does this role exist,
what grants does it have) lives in the target database, not our k8s cluster.

The control loop I see is:
1. Fetch the `Sprout`. If it's being deleted, jump to teardown (below).
2. Ensure the finalizer (`db.sproutdb.dev/finalizer`) is present.
3. Look up the `Provisioner` for `spec.provider`.
4. Resolve `spec.connection.adminSecretRef` into admin credentials.
5. Derive the database name deterministically from `namespace/name` (or
   use `spec.databaseName` if set); this never depends on `status`, so
   it's stable across controller restarts even if status gets lost.
6. `EnsureDatabase`: idempotent create-if-missing, stamped with an
   ownership marker (see below).
7. Check whether the credentials Secret exists in the `Sprout`'s
   namespace.
   - **Missing** (first reconcile, or deleted out-of-band) → treated as
     a rotation request: `EnsureRole` generates a new password, and the
     Secret is created.
   - **Present** → password is left alone; `EnsureRole` still runs to
     reconcile mutable role properties (currently `connectionLimit`). The
     Secret's connection fields (`PGHOST`, `PGPORT`, `PGSSLMODE`,
     `DATABASE_URL`, …) are updated if they drifted from the current spec,
     using the password already stored in the Secret.
8. Update `status` (phase, conditions, derived names,
   `observedGeneration`).
9. Requeue after `resyncInterval` (5m) regardless of outcome. This catches
   state that changed directly against the target database (someone manually
   drops the role, revokes a grant) with no Kubernetes-side event to react to.

**Teardown** (on deletion, while the finalizer is present): resolve the
connection, call `Provisioner.Teardown` (drops role and database once
ownership checks out), then remove the finalizer. If the admin Secret is
already gone too, cleanup is skipped and the finalizer is removed
anyway. Best effort, once there's no way to reach the target instance at all.

### Ownership marker: why it exists

Before `EnsureDatabase`/`Teardown` touch anything, they check an
ownership marker (`COMMENT ON DATABASE <name> IS
'sprout-owner=<CR-UID>'`, stamped at creation) against the reconciling
`Sprout`'s UID. Without this, a deterministic name that happens to
collide with something already there (a typo, a hand-created database, a
second cluster pointed at the same instance) would get silently adopted,
and later **dropped** on teardown. Terraform solves the same problem by
tracking what it created; PV/PVC solve it with `claimRef` and a reclaim
policy. If the marker's missing or doesn't match, the `Sprout` goes to
`Failed` instead of touching an object it doesn't recognize.

### Partial-failure recovery

Every step does its own existence check instead of assuming a clean
slate, so a failure partway through (say the database gets created but
the role doesn't) just means the next reconcile skips what's already
done and picks up at the failure point.

## Naming / identifiers

Database and role names are derived deterministically:
`sprout_<sanitized namespace_name, truncated>_<8-char hash>`, with the
role name just being the database name plus `_role`. Deriving the name
instead of only storing it in `status` means a lost or corrupted
`status` can't make the controller forget what it's managing. It just
recomputes the same name every time. Plenty of room left under
Postgres's 63-byte identifier limit too.

## Secrets

Published in the `Sprout`'s namespace, owned via `ownerReference` so it
gets garbage collected on `Sprout` deletion independent of the
finalizer/teardown path. Ships both a full DSN and the discrete pieces,
since some apps want one and some want the other:

```
DATABASE_URL=postgresql://user:pass@host:5432/dbname?sslmode=prefer
PGHOST=host
PGPORT=5432
PGUSER=user
PGPASSWORD=pass
PGDATABASE=dbname
PGSSLMODE=prefer
```

`sslmode` / `PGSSLMODE` follow `spec.connection.sslMode`.

## Provider abstraction

```go
// internal/provisioner/provisioner.go
type Provisioner interface {
    EnsureDatabase(ctx, conn, spec) error
    EnsureRole(ctx, conn, spec, rotate bool) (Credentials, error)
    Teardown(ctx, conn, spec, roleName string) error
}
```

Just a Go interface, picked at runtime via `spec.provider` through a
`map[string]Provisioner` on the reconciler.

## Explicitly out of scope (v1)

- **Migrations**: the app's own Deployment/Job/initContainer runs its
  own migration tool against the Secret Sprout publishes. Sprout has no
  opinion on what tooling that is.
- **Data cloning/seeding**: see "What this deliberately is not," above.
  The `Provisioner` interface's spec types leave room for an optional
  `seedFrom` later without a breaking change, but nothing implements it.
- **TTL / auto-expiry**: lifecycle is entirely manual/CI-triggered,
  create the `Sprout` when the PR opens, delete it when the PR closes.
  No controller-driven expiry yet.
- **Admission webhook**: CEL validation handles what we need today.
- **Provisioning the underlying instance itself**: Sprout only ever
  talks to something that already exists.

## Open / defaulted-without-deep-deliberation

I've got some defaults here that make sense for now. How they'll scale, dunno:

- Periodic resync interval: **5 minutes**.
- Default `connectionLimit` when unset: **20**.
- Default `connection.sslMode` when unset: **prefer**.
- Default `connection.adminDatabase` when unset: **postgres**.

## Testing

`internal/provisioner/postgres` has an automated conformance suite
(`postgres_test.go`) that spins up a real `postgres:16` container via
`testcontainers-go` and runs it through everything `Provisioner`
promises: idempotency, the ownership-marker safety net on both mutation
and teardown, password rotation, and partial-failure resumability. That
battery is defined once, provider-agnostically, in
`internal/provisioner/conformance`. The only piece a new provider needs
to implement is the `Inspector` interface; `postgres.Inspector`
(`internal/provisioner/postgres/inspector.go`) is exported specifically
so a future Postgres-wire-compatible provider (Aurora, Cloud SQL) can
reuse it instead of rewriting the same catalog queries.

`internal/controller` has an `envtest` suite covering the reconcile
loop's Kubernetes-side mechanics.

## Roadmap (not yet built)

- Additional `Provisioner` implementations (Aurora, Cloud SQL): reuse
  the conformance suite, see Testing above.
- TTL/auto-expiry, if manual lifecycle turns out to actually be a pain
  in practice.
- Prometheus metrics (provisioning latency, error rate, live tenant
  count) and Kubernetes Events for the reconcile steps. Logs alone
  aren't much to build dashboards or alerts on.
