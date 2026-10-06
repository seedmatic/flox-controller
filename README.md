# flox-controller

A **`FloxEnv` CRD** + a **node-agent controller** that delivers flox environments to
Kubernetes nodes **at runtime** — pure-k8s, reusable, and independent of any specific
cluster's conventions.

## What it is

- **`FloxEnv` (CRD, group `flox.seedmatic.io`)** — a flox environment expressed as a
  first-class k8s resource. `spec` carries the flox `manifest.toml` + committed
  `manifest.lock` (self-describing), a `folder` (the relative host-layout path,
  dissociated from the k8s namespace; defaults to the namespace), and `includes`
  (references to other `FloxEnv` CRs, for flox `[include]` composition). `status`
  reports per-node realisation.
- **flox-controller** — a **node-agent** (one instance per node, run as a DaemonSet).
  It watches `FloxEnv` CRs and reconciles each node's local nix store: it tells the
  node's nix daemon to realise the env closures onto the host `/nix/store`, writes the
  `flox-runtime` GC-roots, renders the `[include]` layout, and patches
  `status.realized[node]`. It never re-locks — it realises what the CR carries.

It knows only `(namespace, name)` — never any cluster-specific "domain" concept. The
NRI plugin (a separate component) resolves a pod's `flox.dev/environment` annotation as
a reference to a `FloxEnv` CR and overlays the host `/nix` into the container.

Design: see the rke2lab spec *Flox Store-Resolved Runtime* §Runtime env delivery.

## Consuming it

A nix flake input; the consumer bakes the controller image (foundation) and applies
the CRD + `FloxEnv` CRs.

```nix
inputs.flox-controller.url = "github:seedmatic/flox-controller";
```

## Development

This is a kubebuilder-style project. Generated code (`zz_generated.deepcopy.go`) and
the CRD YAML (`config/crd`) are produced by `controller-gen`; the module's `go.sum` and
the flake's `vendorHash` must be filled before `nix build` is green:

```sh
nix develop
controller-gen object paths=./api/...
controller-gen crd paths=./api/... output:crd:dir=config/crd/bases
go mod tidy        # generates go.sum
# then set flake.nix vendorHash to the hash `nix build` reports
```

Release = bump `VERSION` + tag `vX.Y.Z`.
