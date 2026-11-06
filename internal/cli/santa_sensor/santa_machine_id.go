package santa_sensor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"howett.net/plist"
)

// machinePlistFiles are read in order; the first that exists is used. The example Santa configuration profile names
// the first one for both MachineIDPlist and MachineOwnerPlist. Macs set up from the older example profile still have
// the com.google.santa file.
var machinePlistFiles = []string{
	"/Library/Preferences/com.northpolesec.santa.machine-mapping.plist",
	"/Library/Preferences/com.google.santa.machine-mapping.plist",
}

func GetMyMachineUUID() (string, error) {
	return machineUUIDFrom(machinePlistFiles)
}

// machineUUIDFrom reads MachineUUID from the first of paths that exists. The file may be an XML or a binary plist
// (`defaults write` writes binary). A file that exists without a MachineUUID is reported rather than skipped, so a
// broken mapping file is not silently masked by an older one.
func machineUUIDFrom(paths []string) (string, error) {
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("could not read this Mac's machine ID from %s (%w); pass --machine <machine-id> or --global", path, err)
		}

		var doc struct {
			MachineUUID string `plist:"MachineUUID"`
		}
		// A file that is not a plist dictionary has no MachineUUID either, which is the error below
		_, _ = plist.Unmarshal(raw, &doc)

		if doc.MachineUUID == "" {
			return "", fmt.Errorf("no MachineUUID in %s; pass --machine <machine-id> or --global", path)
		}
		return doc.MachineUUID, nil
	}
	return "", fmt.Errorf("could not find this Mac's machine ID: none of %s exists; pass --machine <machine-id> or --global", strings.Join(paths, ", "))
}
