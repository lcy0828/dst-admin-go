# Kubernetes experimental runtime boundary

This directory contains only the namespace and least-privilege identities for
the experimental Kubernetes runtime Provider. It does not make Kubernetes
runtime control production-ready and it does not register an API route.

`namespace-rbac.yaml` creates two separate ServiceAccounts:

- `dst-admin-kubernetes-provider` is for the future typed Kubernetes API
  adapter. Its Role is namespace-scoped and has no Pod exec/attach, Secret,
  Job, Node, drain, cluster-wide, delete, or arbitrary workload permission.
- `dst-admin-runtime` is used by Shard Pods. It has no RoleBinding, and generated
  Pods set `automountServiceAccountToken: false`.

Kubernetes RBAC cannot restrict writes by label. The typed Driver and a verified
admission layer must both enforce
`app.kubernetes.io/managed-by=dst-admin` plus the Provider/Room/World ownership
labels. Do not attest the start capabilities until that admission layer and the
lease-aware runtime supervisor are installed and tested.

The generated Shard workload references a deterministic, pre-provisioned Room
credential Secret. This Role intentionally cannot read or create Secrets. An
administrator or a separate secret-delivery controller must provision it.

The adapter must use the StatefulSet `scale` subresource for `stop`; it must not
replace the Pod template while stopping a Shard. Create/reconcile operations
should use UID/resourceVersion preconditions and server-side apply.

Inspect the resources before applying them:

```sh
kubectl kustomize deploy/kubernetes
kubectl auth can-i --as=system:serviceaccount:dst-admin-runtime:dst-admin-kubernetes-provider --list -n dst-admin-runtime
```

The first command is read-only. Applying these resources alone is not enough to
start a Shard; see `docs/kubernetes-runtime-experimental.md` for the admission,
storage, network, CPU and failure-injection acceptance gates.
