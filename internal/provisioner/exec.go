package provisioner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/seedmatic/flox-controller/internal/floxenv"
)

// ExecProvisioner realises envs by shelling to the node's flox/nix/containerd.
//
//   - EnvRoot: where .flox source dirs materialise (<EnvRoot>/<folder>/<name>);
//     flox `[include]` paths (../../<folder>/<name>) resolve within this root.
//   - GcrootBase: the flox-runtime GC-root dir the NRI plugin reads
//     (/nix/var/nix/gcroots/flox-runtime/env — floxEnvGcrootBase in the plugin).
//   - CtrBin: the containerd CLI used to import carrier images (rke2 ships it at
//     /var/lib/rancher/rke2/bin/ctr — not on the default PATH).
//   - ContainerdAddress: the containerd socket ctr talks to (rke2 = /run/k3s/…); passed as
//     ctr --address, so the import lands in the node's containerd, not ctr's default socket.
//   - Nsenter: when non-empty, flox/ctr are exec'd through `nsenter -t 1 -m -p -n --` so they
//     run in the HOST's namespaces (needed inside the DaemonSet, where host tools aren't
//     reachable from the container). Empty when the controller runs directly on the node (the
//     nix-run) — tools are found natively. main.go auto-detects which (mount-namespace vs host
//     PID 1). --net (host network) matters for flox: nix fetches the FloxCatalog's flake from the
//     Flux artifact's in-cluster ClusterIP, and host-originated traffic reaches ClusterIPs
//     (Cilium) while BYPASSING the pod NetworkPolicies that otherwise deny a non-flux-system pod
//     the source-controller artifact port (the pod netns times out).
type ExecProvisioner struct {
	EnvRoot           string
	GcrootBase        string
	CtrBin            string
	ContainerdAddress string
	Nsenter           string
}

// command builds an *exec.Cmd for a host tool (flox/ctr). When Nsenter is set the tool runs in
// the host's mount+pid namespaces (container case); otherwise it is exec'd directly (host case).
// The caller's env (see floxCommandEnv) is preserved either way — nsenter does not reset it.
func (p *ExecProvisioner) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	if p.Nsenter != "" {
		full := append([]string{"--target", "1", "--mount", "--pid", "--net", "--", name}, args...)
		return exec.CommandContext(ctx, p.Nsenter, full...)
	}
	return exec.CommandContext(ctx, name, args...)
}

func (p *ExecProvisioner) envDir(ref EnvRef) string {
	return filepath.Join(p.EnvRoot, ref.Folder, ref.Name)
}

func (p *ExecProvisioner) gcrootPath(ref EnvRef) string {
	return filepath.Join(p.GcrootBase, ref.Folder, ref.Name)
}

// activationGcrootPath is the SEPARATE gcroot tree that keeps an env's activation closure
// alive for the container cache-hit — a sibling of GcrootBase (…/flox-runtime/env →
// …/flox-runtime/activation), mirroring the baked model. The NRI plugin never reads it; only
// the env-subtree gcroot (gcrootPath) is plugin-facing.
func (p *ExecProvisioner) activationGcrootPath(ref EnvRef) string {
	return filepath.Join(filepath.Dir(p.GcrootBase), "activation", ref.Folder, ref.Name)
}

// Realize materialises the env source, builds it on the node, writes the GC-root,
// and (image) containerizes + imports.
func (p *ExecProvisioner) Realize(ctx context.Context, req RealizeRequest) (RealizeResult, error) {
	dir := p.envDir(req.Ref)
	floxDir := filepath.Join(dir, ".flox")
	floxEnv := filepath.Join(floxDir, "env")
	if err := os.MkdirAll(floxEnv, 0o755); err != nil {
		return RealizeResult{}, fmt.Errorf("mkdir %s: %w", floxEnv, err)
	}
	if err := os.WriteFile(filepath.Join(floxEnv, "manifest.toml"), req.ManifestTOML, 0o644); err != nil {
		return RealizeResult{}, fmt.Errorf("write manifest.toml: %w", err)
	}
	// flox requires .flox/env.json (the env identity marker) alongside env/manifest.toml —
	// without it `flox activate` fails "unable to locate an env.json". `flox init` writes it;
	// we materialise the whole .flox by hand, so we write it too.
	envJSON := fmt.Sprintf("{\"name\": %q, \"version\": 1}\n", req.Ref.Name)
	if err := os.WriteFile(filepath.Join(floxDir, "env.json"), []byte(envJSON), 0o644); err != nil {
		return RealizeResult{}, fmt.Errorf("write env.json: %w", err)
	}
	// The node-agent never locks: it writes status.lock verbatim and realises it.
	if req.Lock != "" {
		if err := os.WriteFile(filepath.Join(floxEnv, "manifest.lock"), []byte(req.Lock), 0o644); err != nil {
			return RealizeResult{}, fmt.Errorf("write manifest.lock: %w", err)
		}
	}

	// Realise the activation closure the injected container needs. The pod command is a
	// bare `flox activate` (no --mode) — which flox runs in DEV mode (authoritative: the
	// baked nixos/flox-runtime.nix) — so the DEV closure is what the container cache-hits
	// against. Realise it here, where the controller has the memory headroom the pod's
	// cgroup lacks (the whole point of node-side realisation). "run" has no consumer.
	activation, err := p.buildEnv(ctx, dir, "dev")
	if err != nil {
		return RealizeResult{}, err
	}

	// TWO artifacts, mirroring the baked model (nixos/flox-runtime.nix):
	//   1. the env SUBTREE — a store path exposing env/{manifest.toml,manifest.lock} — is
	//      what the NRI plugin readlinks + stats and the env-link hook symlinks; gcroot IT
	//      at the plugin's well-known <GcrootBase>/<folder>/<name> path. NOT the activation
	//      closure (whose manifest.lock sits at its ROOT, no env/ subtree — the CreateContainer
	//      failure that blocked kdns).
	//   2. the activation closure — kept GC-protected under a SEPARATE gcroot tree so the
	//      container's `flox activate` is a cache-hit, never a from-scratch (OOM-prone) build.
	subtree, err := p.addEnvSubtree(ctx, req.Ref, floxEnv)
	if err != nil {
		return RealizeResult{}, err
	}
	if err := p.writeGcroot(p.gcrootPath(req.Ref), subtree); err != nil {
		return RealizeResult{}, err
	}
	if err := p.writeGcroot(p.activationGcrootPath(req.Ref), activation); err != nil {
		return RealizeResult{}, err
	}
	if req.Consumption == "image" {
		if err := p.containerizeAndImport(ctx, dir); err != nil {
			return RealizeResult{}, err
		}
	}
	// Capture the lock flox settled on (the one we wrote if req.Lock was set, else the one
	// activate produced) so the reconciler can pin it into status.lock.
	lock, _ := os.ReadFile(filepath.Join(floxEnv, "manifest.lock"))
	return RealizeResult{
		StorePath:  subtree,
		EnvPath:    dir,
		GcrootPath: p.gcrootPath(req.Ref),
		Lock:       string(lock),
	}, nil
}

// addEnvSubtree stages env/{manifest.toml,manifest.lock} into a clean dir under the env dir
// (on the shared hostPath, so the node's nix sees it through nsenter) and `nix-store --add`s
// it, yielding an immutable /nix/store/<hash>-flox-env-<folder>-<name>-subtree whose env/
// subdir is EXACTLY what the NRI plugin stats (resolveFloxEnvironment) and the env-link hook
// symlinks (.flox/env -> <store>/env). The store-path name is deterministic (basename of the
// staged dir), mirroring the baked mkEnvSubtree. `nix-store --add` (stable CLI) avoids any
// dependency on the nix-command experimental feature being enabled on the node.
func (p *ExecProvisioner) addEnvSubtree(ctx context.Context, ref EnvRef, floxEnv string) (string, error) {
	name := fmt.Sprintf("flox-env-%s-%s-subtree", ref.Folder, ref.Name)
	staging := filepath.Join(p.envDir(ref), name)
	stagingEnv := filepath.Join(staging, "env")
	if err := os.RemoveAll(staging); err != nil {
		return "", fmt.Errorf("clean subtree staging %s: %w", staging, err)
	}
	defer os.RemoveAll(staging)
	if err := os.MkdirAll(stagingEnv, 0o755); err != nil {
		return "", fmt.Errorf("mkdir subtree staging %s: %w", stagingEnv, err)
	}
	for _, f := range []string{"manifest.toml", "manifest.lock"} {
		b, err := os.ReadFile(filepath.Join(floxEnv, f))
		if err != nil {
			return "", fmt.Errorf("read %s for subtree: %w", f, err)
		}
		if err := os.WriteFile(filepath.Join(stagingEnv, f), b, 0o644); err != nil {
			return "", fmt.Errorf("write subtree %s: %w", f, err)
		}
	}
	cmd := p.command(ctx, "nix-store", "--add", staging)
	cmd.Env = floxCommandEnv()
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("nix-store --add %s: %w", staging, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// floxCommandEnv is the environment for flox subprocesses: the process env (which carries
// FLOX_FLOXHUB_TOKEN when the DaemonSet wires it from the replicated Secret) plus the canonical
// flox behavioural knobs — the single source in floxenv, shared with the pod-injecting webhook.
func floxCommandEnv() []string {
	return append(os.Environ(), floxenv.Environ()...)
}

// buildEnv activates the env in the given mode (which locks-if-needed + realises its
// closure onto the host store) and resolves the built store path from the run symlink.
//
// flox materialises ONE run symlink per activation mode (verified on a live node):
//
//	.flox/run/<system>.<name>-run -> /nix/store/…-environment-run  (lean: skips [profile] scaffolding)
//	.flox/run/<system>.<name>-dev -> /nix/store/…-environment-dev  (dev shell scaffolding)
//
// Both carry the SAME packages — mode is activation scaffolding, NOT the package set
// (the flavor lives in which flake output is installed). Select the requested mode's
// symlink by its "-<mode>" suffix; do NOT return the first /nix/store entry — it sorts
// to "-dev" alphabetically, which would gcroot the dev scaffolding into a workload pod.
func (p *ExecProvisioner) buildEnv(ctx context.Context, dir, mode string) (string, error) {
	cmd := p.command(ctx, "flox", "activate", "--mode", mode, "-d", dir, "--", "true")
	cmd.Env = floxCommandEnv()
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("flox activate (--mode %s) %s: %w", mode, dir, err)
	}
	runDir := filepath.Join(dir, ".flox", "run")
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", runDir, err)
	}
	var fallback string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		target, err := os.Readlink(filepath.Join(runDir, e.Name()))
		if err != nil || !strings.HasPrefix(target, "/nix/store/") {
			continue
		}
		if strings.HasSuffix(e.Name(), "-"+mode) {
			return target, nil
		}
		fallback = target
	}
	if fallback != "" {
		return fallback, nil // single-mode env: no per-mode suffix present
	}
	return "", fmt.Errorf("no /nix/store env path under %s", runDir)
}

// writeGcroot atomically points the given gcroot path at the store path. For the plugin-facing
// env gcroot (gcrootPath) that's the subtree; for the activation gcroot it's the closure.
func (p *ExecProvisioner) writeGcroot(gc, storePath string) error {
	if err := os.MkdirAll(filepath.Dir(gc), 0o755); err != nil {
		return fmt.Errorf("mkdir gcroot dir: %w", err)
	}
	tmp := gc + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(storePath, tmp); err != nil {
		return fmt.Errorf("symlink gcroot: %w", err)
	}
	if err := os.Rename(tmp, gc); err != nil {
		return fmt.Errorf("rename gcroot: %w", err)
	}
	return nil
}

// containerizeAndImport builds the carrier OCI image on the node and imports it
// into containerd's k8s.io namespace.
func (p *ExecProvisioner) containerizeAndImport(ctx context.Context, dir string) error {
	tar := filepath.Join(dir, ".flox", "containerize.tar")
	build := p.command(ctx, "flox", "containerize", "-d", dir, "-f", tar)
	build.Env = floxCommandEnv()
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("flox containerize %s: %w", dir, err)
	}
	ctr := p.CtrBin
	if ctr == "" {
		ctr = "ctr"
	}
	args := make([]string, 0, 6)
	if p.ContainerdAddress != "" {
		args = append(args, "--address", p.ContainerdAddress)
	}
	args = append(args, "-n", "k8s.io", "images", "import", tar)
	imp := p.command(ctx, ctr, args...)
	imp.Stderr = os.Stderr
	if err := imp.Run(); err != nil {
		return fmt.Errorf("ctr images import: %w", err)
	}
	return nil
}

// Prune is the sole-writer cleanup of layout + GC-roots for undesired envs.
// INCREMENT 2: walk GcrootBase + EnvRoot and remove entries absent from keep.
func (p *ExecProvisioner) Prune(ctx context.Context, keep []EnvRef) error {
	return nil
}
