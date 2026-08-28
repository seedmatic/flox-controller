package provisioner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
type ExecProvisioner struct {
	EnvRoot           string
	GcrootBase        string
	CtrBin            string
	ContainerdAddress string
}

func (p *ExecProvisioner) envDir(ref EnvRef) string {
	return filepath.Join(p.EnvRoot, ref.Folder, ref.Name)
}

func (p *ExecProvisioner) gcrootPath(ref EnvRef) string {
	return filepath.Join(p.GcrootBase, ref.Folder, ref.Name)
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

	storePath, err := p.buildEnv(ctx, dir)
	if err != nil {
		return RealizeResult{}, err
	}
	if err := p.writeGcroot(req.Ref, storePath); err != nil {
		return RealizeResult{}, err
	}
	if req.Consumption == "image" {
		if err := p.containerizeAndImport(ctx, dir); err != nil {
			return RealizeResult{}, err
		}
	}
	return RealizeResult{StorePath: storePath}, nil
}

// buildEnv activates the env (which locks-if-needed + realises its closure onto the
// host store) and resolves the built store path from the run symlink.
//
// ON-NODE TBD: the exact flox invocation + run-symlink layout is to be pinned
// against a live node; `flox activate -- true` forces a build and leaves
// .flox/run/<system>.<name> -> /nix/store/...-environment. The reconcile loop and
// the GC-root contract around it are correct regardless of this detail.
func (p *ExecProvisioner) buildEnv(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "flox", "activate", "-d", dir, "--", "true")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("flox activate %s: %w", dir, err)
	}
	runDir := filepath.Join(dir, ".flox", "run")
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", runDir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		target, err := os.Readlink(filepath.Join(runDir, e.Name()))
		if err == nil && strings.HasPrefix(target, "/nix/store/") {
			return target, nil
		}
	}
	return "", fmt.Errorf("no /nix/store env path under %s", runDir)
}

// writeGcroot atomically points <GcrootBase>/<folder>/<name> at the store path —
// the exact path the NRI plugin readlinks to resolve the env.
func (p *ExecProvisioner) writeGcroot(ref EnvRef, storePath string) error {
	gc := p.gcrootPath(ref)
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
	build := exec.CommandContext(ctx, "flox", "containerize", "-d", dir, "-f", tar)
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
	imp := exec.CommandContext(ctx, ctr, args...)
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
