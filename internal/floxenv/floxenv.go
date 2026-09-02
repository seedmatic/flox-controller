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

import "strings"

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
		"build-users-group =",
		"sandbox = false",
		"min-free = 5368709120",  // 5 GiB
		"max-free = 10737418240", // 10 GiB
	}, "\n")
}
