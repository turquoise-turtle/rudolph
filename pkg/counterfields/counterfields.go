// Package counterfields says which display fields (file name, signing ID, team ID) a day counter of the events table
// may carry. The event writer (internal/handlers/eventupload) and the ranker (internal/ranker) share it: the writer
// so that counters stay small, the ranker so that counters written before the writer checked, which hold values of up
// to 4,096 bytes, cannot fill its memory. An unusable value is left out whole, never cut: the ranker looks global
// rules up by signing ID and team ID, and a cut value could name another rule.
package counterfields

const (
	// MaxFileNameBytes is the longest file name macOS allows (NAME_MAX), so no real binary has a longer one.
	MaxFileNameBytes = 255

	// MaxSigningIDBytes is far above any real signing ID (a team ID, a colon and a bundle identifier, usually under
	// 100 bytes), and keeps what the ranker holds for a binary small.
	MaxSigningIDBytes = 255

	// TeamIDBytes is the length of every Apple team ID: 10 upper-case letters or digits.
	TeamIDBytes = 10
)

// FileName returns the name if a counter may carry it, else "".
func FileName(name string) string {
	if len(name) > MaxFileNameBytes {
		return ""
	}
	return name
}

// SigningID returns the signing ID if a counter may carry it, else "".
func SigningID(id string) string {
	if len(id) > MaxSigningIDBytes {
		return ""
	}
	return id
}

// TeamID returns the team ID if a counter may carry it, else "".
func TeamID(id string) string {
	if len(id) != TeamIDBytes {
		return ""
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return ""
		}
	}
	return id
}
