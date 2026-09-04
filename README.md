# Sprout

**Sprout** is a Kubernetes operator that gives every pull request its own
isolated Postgres database, created fresh, migrated from scratch by your
own tooling, and torn down automatically when you delete the `Sprout`
object. Point it at any Postgres backend through a small provider interface.

Sprout doesn't do data branching or cloning (see [DESIGN.md](DESIGN.md)
if you want the reasoning). What you get instead is a clean, empty,
isolated database, and then Sprout gets out of your way.

## Getting Started

### Prerequisites
- go version v1.24.6+
- docker version 17.03+.
- kubectl version v1.11.3+.
- [kind](https://kind.sigs.k8s.io/): only needed for [Local
  Development](#local-development), below. Not required to deploy to a
  real cluster.
- Access to a Kubernetes v1.11.3+ cluster.

## Local Development

Spin up a throwaway kind cluster and Postgres instance, and run the
controller straight from your host. No image build, no registry, no
real cluster needed. This is the fastest way to iterate on the
controller or the `postgres` `Provisioner`.

1. **Bring up the cluster, CRD, and Postgres:**

   ```sh
   make local-up
   ```

   Creates a kind cluster named `sprout-dev`, installs the `Sprout` CRD,
   and deploys `hack/local/postgres.yaml`, a throwaway, single-replica
   Postgres with no persistence and a hardcoded admin password. Good for
   nothing but local testing.

2. **Forward Postgres to your host**, in its own terminal (the
   controller runs on your host via `make run`, not inside the cluster,
   so it needs a way to reach the Postgres pod):

   ```sh
   kubectl port-forward svc/postgres 5432:5432
   ```

3. **Run the controller**, in another terminal:

   ```sh
   make run
   ```

4. **Apply the local sample `Sprout`** (`hack/local/sprout-sample.yaml`,
   the same as `config/samples/db_v1alpha1_sprout.yaml`, but pointed at
   `127.0.0.1` to match the port-forward instead of the in-cluster
   Service DNS name):

   ```sh
   kubectl apply -f hack/local/sprout-sample.yaml
   ```

5. **Watch it reconcile:**

   ```sh
   kubectl get sprout sprout-local-sample -w
   ```

   `PHASE` should hit `Ready` within a few seconds. Once it does, the
   credentials are sitting in a Secret with the same name
   (`kubectl get secret sprout-local-sample -o yaml`), and you can go
   check the database directly:

   ```sh
   kubectl exec deploy/postgres -- psql -U postgres -c '\l'
   ```

   > **Heads up:** don't use this port-forward tunnel to test *password*
   > enforcement (e.g. that rotation actually invalidates the old
   > password). The official Postgres image trusts any connection that
   > looks like it's coming from `127.0.0.1` inside its own pod, and
   > `kubectl port-forward` makes host connections look exactly like
   > that, so a wrong password will falsely seem to work. To actually
   > test password auth, connect from *inside* the cluster instead, e.g.
   > `kubectl run pg-client --rm -i --restart=Never --image=postgres:16
   > -- psql "postgresql://<user>:<password>@postgres:5432/<db>"` (via
   > the `postgres` Service DNS name, not `127.0.0.1`).

6. **Tear down the `Sprout`** and confirm the database/role are dropped:

   ```sh
   kubectl delete -f hack/local/sprout-sample.yaml
   ```

7. **When you're done**, Ctrl-C the port-forward and `make run`, then
   tear down the cluster:

   ```sh
   make local-down
   ```

### To Deploy on the cluster

**Build and push your image** to wherever `IMG` points:

```sh
make docker-build docker-push IMG=<some-registry>/sprout:tag
```

Make sure that image is actually reachable from wherever you're
deploying. If these commands fail, double-check your registry
permissions.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/sprout:tag
```

**Create a `Sprout`.** You can apply the sample from `config/samples`:

```sh
kubectl apply -k config/samples/
```

### To Uninstall

**Delete the `Sprout` instances from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the CRDs:**

```sh
make uninstall
```

**Undeploy the controller:**

```sh
make undeploy
```

## Project Distribution

A couple of ways to actually ship.

### As a YAML bundle

1. Build the installer for the image you've built and published:

   ```sh
   make build-installer IMG=<some-registry>/sprout:tag
   ```

   This generates `dist/install.yaml`: everything Kustomize built,
   bundled into one file, with no other dependencies needed to install.

2. Users install it with a single `kubectl apply`:

   ```sh
   kubectl apply -f https://raw.githubusercontent.com/<org>/sprout/<tag or branch>/dist/install.yaml
   ```

### As a Helm chart

1. Generate the chart with the optional Helm plugin:

   ```sh
   kubebuilder edit --plugins=helm/v2-alpha
   ```

2. The chart lands under `dist/chart`. That's what you'd publish.

   If you change the project later, rerun the command above to keep the
   chart in sync (add `--force` if you've added webhooks, and manually
   reapply any custom values you'd set in `dist/chart/values.yaml` or
   `dist/chart/manager/manager.yaml`; the force flag overwrites them).

## Contributing
TODO I suppose. Just yap in the Issues for now.

**NOTE:** Run `make help` for more information on all potential `make` targets

## License

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
