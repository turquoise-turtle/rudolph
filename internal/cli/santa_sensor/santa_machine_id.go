package santa_sensor

import (
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"os"
)

const (
	// This should match the MachineOwnerPlist file, specified in the Santa configuration profile
	defaultSantaMachinePlistFile = "/Library/Preferences/com.google.santa.machine-mapping.plist"
)

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

func GetMyMachineUUID() (machineUUID string, err error) {
	xmlFile, err := os.Open(defaultSantaMachinePlistFile)
	if err != nil {
		err = fmt.Errorf("could not read this Mac's machine ID from %s (%w); pass --machine <machine-id> or --global", defaultSantaMachinePlistFile, err)
		return
	}

	defer xmlFile.Close()

	byteValue, _ := ioutil.ReadAll(xmlFile)

	var doc santaPlistXMLDocument
	// A plist that is not XML has no MachineUUID either, which is the error below
	_ = xml.Unmarshal(byteValue, &doc)

	if len(doc.Data.Keys) == len(doc.Data.Strings) {
		for i, key := range doc.Data.Keys {
			if key == "MachineUUID" {
				machineUUID = doc.Data.Strings[i]
			}
		}
	}

	if machineUUID == "" {
		err = fmt.Errorf("no MachineUUID in %s; pass --machine <machine-id> or --global", defaultSantaMachinePlistFile)
	}

	return
}
