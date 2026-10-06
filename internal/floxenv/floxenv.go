// Package floxenv is the SINGLE SOURCE of the flox behavioural settings this system applies
// wherever flox runs. Defined once here, projected onto every vector that invokes flox:
//   - the controller's own flox subprocesses (provisioner.floxCommandEnv);
//   - the mutating webhook that injects them into flox-consuming pods — subsuming the NRI
//     plugin's ad-hoc AddEnv and the flox-runtime ConfigMap envFrom (a later increment).
//
// The FloxHub auth token is deliberately NOT here: it is per-deployment secret material carried
// through the process/pod environment (FLOX_FLOXHUB_TOKEN, sourced from a replicated Secret), not
// a static behavioural knob.
package floxenv

import (
	"sort"
	"strings"
)

const (
	// AnnotationPrefix is the per-container opt-in the NRI plugin keys on:
	// flox.seedmatic.io/environment.<container> = "<folder>/<name>". The webhook mirrors it to
	// know which envs a pod consumes; the gate reconciler resolves each to a FloxEnv.
	AnnotationPrefix = "flox.seedmatic.io/environment."
	// DefaultCategory is the folder a bare-name annotation value (no "/") resolves against —
	// matches the plugin's bare-name fallback.
	DefaultCategory = "networking"
	// SchedulingGateName is the scheduling gate the webhook adds to a flox-consuming pod; the
	// gate reconciler removes it once every referenced FloxEnv is realised at the current
	// generation. Until then the pod stays SchedulingGated (unscheduled, no container starts).
	SchedulingGateName = "flox.seedmatic.io/env-ready"
)

// EnvRef is a flox env coordinate as an annotation encodes it: the host-layout (folder, name)
// pair, matching a FloxEnv's (spec.folder-or-namespace, metadata.name).
type EnvRef struct {
	Folder string
	Name   string
}

// RefsFromAnnotations resolves every flox.seedmatic.io/environment.<c> annotation to an EnvRef,
// de-duplicated and sorted (stable ordering for idempotent mutation + reconcile). The value is
// "<folder>/<name>"; the name is the LAST segment (a FloxEnv's metadata.name is DNS-1123, never
// contains "/"), so the folder is everything before it — this recovers a STRUCTURED spec.folder
// ("mesh/base") intact, not just its first segment. A bare value (no "/") uses DefaultCategory.
func RefsFromAnnotations(annotations map[string]string) []EnvRef {
	seen := map[EnvRef]struct{}{}
	for k, v := range annotations {
		if !strings.HasPrefix(k, AnnotationPrefix) || v == "" {
			continue
		}
		folder, name := DefaultCategory, v
		if i := strings.LastIndex(v, "/"); i >= 0 {
			folder, name = v[:i], v[i+1:]
		}
		seen[EnvRef{Folder: folder, Name: name}] = struct{}{}
	}
	refs := make([]EnvRef, 0, len(seen))
	for r := range seen {
		refs = append(refs, r)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Folder != refs[j].Folder {
			return refs[i].Folder < refs[j].Folder
		}
		return refs[i].Name < refs[j].Name
	})
	return refs
}

// Setting is one flox behavioural knob — a name/value applied to flox's environment.
type Setting struct {
	Name  string
	Value string
}

// Settings returns the canonical flox behavioural knobs, in a stable order.
func Settings() []Setting {
	return []Setting{
		// Without this, every `flox activate` spawns a DETACHED check-for-upgrades that runs
		// `nix eval --refresh` — it races the .flox lock between rapid reconciles (intermittent
		// `flox activate: exit status 1`) and can OOM a few seconds AFTER activation returns.
		// Internal flox knob (cli/flox/src/commands/check_for_upgrades.rs), the only mechanism
		// today. FRAGILE: not a supported config key; may change or vanish across flox releases.
		{Name: "_FLOX_TESTING_DISABLE_BG_SIDE_EFFECTS", Value: "true"},
		// Quiet metrics/telemetry, and force non-interactive (an unattended agent never prompts).
		{Name: "FLOX_DISABLE_METRICS", Value: "true"},
		{Name: "FLOX_NO_TELEMETRY", Value: "1"},
		{Name: "FLOX_NONINTERACTIVE", Value: "1"},
	}
}

// Environ renders the settings as KEY=VALUE entries, for exec.Cmd.Env / an os.Environ append.
func Environ() []string {
	settings := Settings()
	out := make([]string, 0, len(settings))
	for _, s := range settings {
		out = append(out, s.Name+"="+s.Value)
	}
	return out
}

// NixConfig is the NIX_CONFIG value the flox-nri system injects into a nix-build container. A pod's
// nix is single-user + daemonless with no nix.conf, so the flake commands need these enabled
// explicitly (dev inherits them from the user's global nix.conf):
//
//	experimental-features = nix-command flakes  the flake CLI (`nix run`/`nix build`)
//	build-users-group =                         build as the pod user (no `nixbld` group)
//	sandbox = false                             the unprivileged pod can't set up the build sandbox
//	min-free / max-free                         store GC: bound the persistent nix-store PVC
//
// The GC is nix-NATIVE, not a manual `nix store gc` (which deletes ALL unreferenced paths → wipes
// the warm cache every run). During a build, when the store filesystem's free space drops below
// min-free, nix garbage-collects unreferenced paths (LRU) until max-free is free, then continues.
// On the /nix overlay this reaps unrooted UPPER paths only — the node's read-only lower store is
// fully GC-rooted, so it is untouched and the merged view stays coherent (statvfs on the overlay
// reports the upper PVC's free space, so the thresholds track the PVC's fill). This keeps the store
// bounded WITHOUT wiping the warm cache — it frees only what a build needs. Sized for the 30Gi
// nix-store PVC: GC when < min-free (5 GiB) free, down to max-free (10 GiB), keeping ~20 GiB warm.
//
// NIX_CONFIG merges newline-separated key=value lines and is inherited by nested nix processes, so
// one env var covers the outer `nix run` and the render app's inner `nix build`.
func NixConfig() string {
	return strings.Join([]string{
		"experimental-features = nix-command flakes",
		// Trust the flake's own nixConfig (it declares pure-eval=false so `builtins.getEnv`
		// works). Without this, nix logs "ignoring untrusted flake configuration setting
		// 'pure-eval'" and evaluates PURE → getEnv returns "" → a build that reads host env
		// (e.g. the render's M2_REPO/MAVEN_BUILD_CACHE for its maven cache) silently sees
		// nothing and rebuilds cold every run. Inherited by the inner `nix build`, so the
		// flake's declaration applies without a per-invocation --impure.
		"accept-flake-config = true",
		"build-users-group =",
		"sandbox = false",
		"min-free = 5368709120",  // 5 GiB
		"max-free = 10737418240", // 10 GiB
	}, "\n")
}
