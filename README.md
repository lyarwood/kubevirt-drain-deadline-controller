# kubevirt-drain-deadline-controller

> **⚠️ Experimental — not production-ready**
>
> This is a personal experiment exploring deadline-based node drain for KubeVirt VMs.
> Do not use in production. APIs, behaviour, and design may change without notice.

## What is this?

An external controller that enforces a drain deadline when evacuating KubeVirt
`VirtualMachineInstance` (VMI) objects from a node.

KubeVirt's built-in eviction handling retries live migration indefinitely — there
is no built-in way to say "drain this node by T, and kill anything that hasn't
migrated in time." This controller provides that capability using KubeVirt's
`EvictionStrategy: External` hook.

## How it works

1. VMs are configured with `evictionStrategy: External` in their spec.
2. When a node is drained (`kubectl drain`), KubeVirt's webhook sets
   `vmi.status.evacuationNodeName` and blocks pod eviction — but does **not**
   create a migration.
3. This controller observes `evacuationNodeName` and:
   - Creates a `VirtualMachineInstanceMigration` to attempt live migration.
   - Tracks the deadline from the oldest managed migration's `creationTimestamp`.
   - If the deadline passes and the VMI is still on the source node, **deletes the VMI**.
4. For VMs with `runStrategy: Always` / `RerunOnFailure`, the VM controller
   automatically recreates the VMI on another node. For `Manual` / `Once`, the
   deletion is final (a warning is logged).

## Deadline configuration

Set an absolute RFC3339 deadline annotation on the **Node** before draining:

```bash
kubectl annotate node <node> \
  deadline-eviction.kubevirt.io/drain-deadline=2026-10-01T16:00:00Z

kubectl drain <node> --ignore-daemonsets --delete-emptydir-data
```

If no node annotation is present, the controller falls back to
`--default-drain-deadline-duration` from the time the first migration was
attempted (default: 1 hour).

## VM configuration

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: my-vm
spec:
  runStrategy: Always
  template:
    spec:
      evictionStrategy: External
      # ... rest of VMI spec
```

## Running

```bash
# Deploy the controller into an existing KubeVirt cluster
kubectl apply -k config/default

# Or against a local kubevirtci cluster
make cluster-up
make cluster-sync
```

## Testing

```bash
# Fast fake-client unit tests
make test

# envtest integration tests (real API server, no cluster)
make test-integration

# e2e functional tests (requires a running KubeVirt cluster)
KUBECONFIG=/path/to/kubeconfig make functest

# e2e against local kubevirtci cluster
make cluster-functest
```

## kubevirtci cluster lifecycle

```bash
make cluster-up      # start a local k8s-1.36 cluster with KubeVirt installed
make cluster-sync    # build + push image + deploy controller
make cluster-down    # tear down the cluster
```

## Caveats and known limitations

- `EvictionStrategy: External` means KubeVirt does **not** attempt migration on
  its own — this controller must be running before any drain is initiated.
- If the controller is down when a drain occurs, `evacuationNodeName` will be
  set but no migration will be created and no deadline enforced.
- The deadline is anchored to the first managed migration's `creationTimestamp`,
  not to when the drain was triggered — so controller downtime delays the clock.
- No leader-election-aware graceful handoff between controller replicas during
  rolling updates.

## Project layout

```
pkg/controller/          controller logic + fake-client unit tests
tests/integration/       envtest integration tests
tests/functional/        e2e tests against a live KubeVirt cluster
config/rbac/             RBAC manifests
config/default/          kustomize deployment
hack/kubevirtci.sh       local cluster lifecycle
```

## Related

- [KubeVirt EvictionStrategy docs](https://kubevirt.io/user-guide/)
- [cluster-api-provider-kubevirt](https://github.com/kubernetes-sigs/cluster-api-provider-kubevirt) — the primary consumer of `EvictionStrategy: External` today
