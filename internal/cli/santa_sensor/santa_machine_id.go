package santa_sensor

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// machinePlistFiles are read in order; the first that exists is used. They should match the MachineIDPlist in the
// Santa configuration profile. Macs set up from the older example profile still have the com.google.santa file.
var machinePlistFiles = []string{
	"/Library/Preferences/com.northpolesec.santa.machine-mapping.plist",
	"/Library/Preferences/com.google.santa.machine-mapping.plist",
}

type santaPlistXMLDocument struct {
	XMLName xml.Name            `xml:"plist"`
	Data    santaPlistKeyValues `xml:"dict"`
}

type santaPlistKeyValues struct {
	// Annoyingly, because the plist files use <key> and <string> as the respective values in order, =
	// you have to infer which string maps to which key based upon their array positions.
	Keys    []string `xml:"key"`
	Strings []string `xml:"string"`
}

func GetMyMachineUUID() (string, error) {
	return machineUUIDFrom(machinePlistFiles)
}

// machineUUIDFrom reads MachineUUID from the first of paths that exists. A file that exists without a MachineUUID
// is reported rather than skipped, so a broken mapping file is not silently masked by an older one.
func machineUUIDFrom(paths []string) (string, error) {
	for _, path := range paths {
		byteValue, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("could not read this Mac's machine ID from %s (%w); pass --machine <machine-id> or --global", path, err)
		}

		var doc santaPlistXMLDocument
		// A plist that is not XML has no MachineUUID either, which is the error below
		_ = xml.Unmarshal(byteValue, &doc)

		var machineUUID string
		if len(doc.Data.Keys) == len(doc.Data.Strings) {
			for i, key := range doc.Data.Keys {
				if key == "MachineUUID" {
					machineUUID = doc.Data.Strings[i]
				}
			}
		}
		if machineUUID == "" {
			return "", fmt.Errorf("no MachineUUID in %s; pass --machine <machine-id> or --global", path)
		}
		return machineUUID, nil
	}
	return "", fmt.Errorf("could not find this Mac's machine ID: none of %s exists; pass --machine <machine-id> or --global", strings.Join(paths, ", "))
}
