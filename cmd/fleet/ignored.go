package main

// retiredMinSantaVersionEnv set the minimum Santa version before it became part of the fleet health setting
// (FleetHealth / Config, pkg/model/fleethealth).
const retiredMinSantaVersionEnv = "FLEET_MIN_SANTA_VERSION"

// ignoredEnvironment returns a line to log for each setting that is still in the environment but no longer read, so
// an operator who set one finds out where it went instead of wondering why it has no effect.
func ignoredEnvironment(getenv func(string) string) []string {
	var lines []string
	if getenv(retiredMinSantaVersionEnv) != "" {
		lines = append(lines, "fleet: "+retiredMinSantaVersionEnv+" is set but ignored: the minimum Santa version is now the stored fleet health setting. "+
			"Store it with `rudolph fleet min-santa-version <version>`, then remove the variable from the function.")
	}
	return lines
}
