package santa_sensor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
)

// SantaFileInfo maps to the data output by santactl:
//
//	https://github.com/google/santa/blob/d17aeac2f4331584db51de432ace3465026e1259/Source/santactl/Commands/SNTCommandFileInfo.m#L199-L214
type santaFileInfo struct {
	Path                  string             `json:"Path"`
	SHA256                string             `json:"SHA-256"`
	SHA1                  string             `json:"SHA-1"`
	TeamID                string             `json:"Team ID"`
	SigningID             string             `json:"Signing ID"`
	BundleName            string             `json:"Bundle Name"`
	BundleVersion         string             `json:"Bundle Version"`
	BundleVersionStr      string             `json:"Bundle Version Str"`
	DownloadReferrerURL   string             `json:"Download Referrer URL"`
	DownloadURL           string             `json:"Download URL"`
	DownloadTimestamp     string             `json:"Download Timestamp"`
	DownloadAgent         string             `json:"Download Agent"`
	Type                  string             `json:"Type"`
	PageZero              string             `json:"Page Zero"`
	CodeSigned            string             `json:"Code-signed"`
	Rule                  string             `json:"Rule"`
	SigningChain          []signingChainInfo `json:"Signing Chain"`
	UniversalSigningChain []signingChainInfo `json:"Universal Signing Chain"`
}

// SigningChainInfo maps to the data output by santactl for signing info:
//
//	https://github.com/google/santa/blob/d17aeac2f4331584db51de432ace3465026e1259/Source/santactl/Commands/SNTCommandFileInfo.m#L421-L427
type signingChainInfo struct {
	SHA256             string `json:"SHA-256"`
	SHA1               string `json:"SHA-1"`
	CommonName         string `json:"Common Name"`
	Organization       string `json:"Organization"`
	OrganizationalUnit string `json:"Organizational Unit"`
	ValidFrom          string `json:"Valid From"`
	ValidUntil         string `json:"Valid Until"`
}

func RunSantaFileInfo(filepath string) (santaFileInfo, error) {
	cmd := exec.Command("santactl", "fileinfo", "--json", filepath)

	var fileInfo santaFileInfo
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(bytes.TrimSpace(exitErr.Stderr)) > 0 {
			return fileInfo, fmt.Errorf("santactl fileinfo %s: %w: %s", filepath, err, bytes.TrimSpace(exitErr.Stderr))
		}
		return fileInfo, fmt.Errorf("santactl fileinfo %s: %w", filepath, err)
	}

	var allInfo []santaFileInfo
	err = json.Unmarshal(output, &allInfo)
	if err != nil {
		return fileInfo, fmt.Errorf("could not load file info from santa: %w", err)
	}

	if len(allInfo) == 0 {
		return fileInfo, fmt.Errorf("santactl fileinfo returned no information about %s", filepath)
	}

	// We only support running on one item right now (not directories of items) so get the first one
	fileInfo = allInfo[0]

	return fileInfo, nil
}
