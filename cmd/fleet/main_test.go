package main

// FLEET_MIN_SANTA_VERSION is retired (owner decision, 2026-11-25): the minimum Santa version is the stored fleet health
// setting, changed with `rudolph fleet min-santa-version`, so it takes effect without a redeploy. An environment that
// still sets the variable on the function must keep working, and must be told, once per start of the function, that
// the value is ignored and where the setting now lives.
//
// ignoredEnvironment(getenv) returns the lines to log at start for settings that are still in the environment but no
// longer read: exactly one for a non-empty FLEET_MIN_SANTA_VERSION, naming the variable, saying it is ignored, and
// naming `rudolph fleet min-santa-version`; none when it is unset or empty. main calls ignoredEnvironment(os.Getenv)
// before lambda.Start, logs each line, and carries on: the job must not fail over it.

import (
	"os"
	"strings"
	"testing"
)

func env(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func TestIgnoredEnvironment_MinSantaVersionStillSet(t *testing.T) {
	for _, value := range []string{"2026.10", "off", "garbage"} {
		lines := ignoredEnvironment(env(map[string]string{"FLEET_MIN_SANTA_VERSION": value, "EVENTS_DYNAMODB_NAME": "events", "DYNAMODB_NAME": "store"}))
		if len(lines) != 1 {
			t.Errorf("FLEET_MIN_SANTA_VERSION=%s: %d lines %q, want exactly one", value, len(lines), lines)
			continue
		}
		for _, want := range []string{"FLEET_MIN_SANTA_VERSION", "ignored", "rudolph fleet min-santa-version"} {
			if !strings.Contains(lines[0], want) {
				t.Errorf("FLEET_MIN_SANTA_VERSION=%s: line %q does not mention %q", value, lines[0], want)
			}
		}
	}
}

func TestIgnoredEnvironment_NothingToSay(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"unset": {"EVENTS_DYNAMODB_NAME": "events", "DYNAMODB_NAME": "store", "FLEET_MAX_MACS": "1000"},
		"empty": {"EVENTS_DYNAMODB_NAME": "events", "DYNAMODB_NAME": "store", "FLEET_MIN_SANTA_VERSION": ""},
	} {
		if lines := ignoredEnvironment(env(vars)); len(lines) != 0 {
			t.Errorf("%s: %q, want no lines", name, lines)
		}
	}
}

// main no longer passes the variable to the job, and logs what ignoredEnvironment returns.
func TestMain_DoesNotReadMinSantaVersion(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	main := string(src)
	if strings.Contains(main, `Getenv("FLEET_MIN_SANTA_VERSION")`) || strings.Contains(main, "MinSantaVersion:") {
		t.Error("main.go still reads FLEET_MIN_SANTA_VERSION or passes a MinSantaVersion to the job")
	}
	if !strings.Contains(main, "ignoredEnvironment(os.Getenv)") {
		t.Error("main.go does not call ignoredEnvironment(os.Getenv) to log the settings it ignores")
	}
}
