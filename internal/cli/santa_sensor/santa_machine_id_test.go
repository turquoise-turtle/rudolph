package santa_sensor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The example profile now names /Library/Preferences/com.northpolesec.santa.machine-mapping.plist; Macs set up
// from the older example still have the com.google.santa file. The CLI reads the North Pole Security file first
// and falls back to the old one.
func TestMachinePlistFiles_NorthPoleSecurityFirst(t *testing.T) {
	want := []string{
		"/Library/Preferences/com.northpolesec.santa.machine-mapping.plist",
		"/Library/Preferences/com.google.santa.machine-mapping.plist",
	}
	if len(machinePlistFiles) != len(want) {
		t.Fatalf("machinePlistFiles = %v, want %v", machinePlistFiles, want)
	}
	for i := range want {
		if machinePlistFiles[i] != want[i] {
			t.Errorf("machinePlistFiles[%d] = %q, want %q", i, machinePlistFiles[i], want[i])
		}
	}
}

func writePlist(t *testing.T, dir, name, uuid string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	body := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>MachineUUID</key><string>` + uuid + `</string></dict></plist>`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMachineUUIDFrom_FirstFileThatExistsWins(t *testing.T) {
	dir := t.TempDir()
	nps := filepath.Join(dir, "nps.plist")
	google := writePlist(t, dir, "google.plist", "BBBBBBBB-2222-4222-8222-BBBBBBBBBBBB")

	got, err := machineUUIDFrom([]string{nps, google})
	if err != nil || got != "BBBBBBBB-2222-4222-8222-BBBBBBBBBBBB" {
		t.Errorf("only the old file: %q, %v; want its MachineUUID", got, err)
	}

	writePlist(t, dir, "nps.plist", "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA")
	got, err = machineUUIDFrom([]string{nps, google})
	if err != nil || got != "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA" {
		t.Errorf("both files: %q, %v; want the North Pole Security file's MachineUUID", got, err)
	}
}

func TestMachineUUIDFrom_NoFileNamesEveryPath(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.plist"), filepath.Join(dir, "b.plist")
	_, err := machineUUIDFrom([]string{a, b})
	if err == nil {
		t.Fatal("no error without any plist")
	}
	for _, want := range []string{a, b, "--machine"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A file that exists but has no MachineUUID is a misconfiguration to report, not a reason to read an older file.
func TestMachineUUIDFrom_FileWithoutUUIDIsAnError(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.plist")
	if err := os.WriteFile(empty, []byte(`<plist version="1.0"><dict></dict></plist>`), 0o644); err != nil {
		t.Fatal(err)
	}
	google := writePlist(t, dir, "google.plist", "BBBBBBBB-2222-4222-8222-BBBBBBBBBBBB")
	_, err := machineUUIDFrom([]string{empty, google})
	if err == nil || !strings.Contains(err.Error(), empty) {
		t.Errorf("error %v, want one naming %s", err, empty)
	}
}
