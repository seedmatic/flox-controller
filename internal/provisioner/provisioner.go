// Package provisioner is the host-side seam of reconciliation. The reconciler
// (k8s control loop) drives it; the exec impl shells to the node's flox/nix/ctr,
// and tests substitute a fake. Keeping host ops behind this interface is what lets
// the reconciler be unit-tested without a node.
package provisioner

import "context"

// EnvRef identifies a FloxEnv's host materialisation at <envroot>/<folder>/<name>.
// Folder is spec.folder (defaulted to the namespace by the reconciler); it is also
// what other envs' flox `[include]` paths resolve against.
type EnvRef struct {
	Folder string
	Name   string
}

// RealizeRequest is one env to materialise on this node.
type RealizeRequest struct {
	Ref EnvRef
	// ManifestTOML is spec.manifest serialised to manifest.toml.
	ManifestTOML []byte
	// Lock is status.lock (the resolved manifest.lock). May be empty before a
	// lock-producer has filled it; the node-agent never locks.
	Lock string
	// Consumption is "overlay" or "image".
	Consumption string
}

// RealizeResult reports the realised env store path (what the NRI plugin resolves
// to via the GC-root).
type RealizeResult struct {
	StorePath string
}

// Provisioner performs the host-side reconciliation on the LOCAL node: materialise
// the flox env, realise its closure onto the host /nix/store, write the
// flox-runtime GC-root the NRI plugin reads, and — for image envs — containerize +
// import the carrier into containerd.
type Provisioner interface {
	Realize(ctx context.Context, req RealizeRequest) (RealizeResult, error)
	// Prune removes host layout + GC-roots for envs no longer desired.
	Prune(ctx context.Context, keep []EnvRef) error
}
