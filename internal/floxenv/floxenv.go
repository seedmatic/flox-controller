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
