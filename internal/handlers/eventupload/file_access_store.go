package eventupload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
)

// File access (FAA) events share the events table with executions. They have no day counter or machine marker: the
// web view only lists them, by machine (their own partition), by fleet and by decision. Their Fleet and Decision
// values are ones no execution has, so they sit apart from executions in those indexes, and the process's team ID is
// stored as ProcessTeamID so that they stay out of TeamID_TS, which lists executions of a team's binaries.
const (
	fileAccessPKPrefix = "FileAccess#"
	fileAccessFleet    = "FileAccess"

	// maxStoredChain bounds the process chain kept: the accessing process and its nearest ancestors are what an
	// admin reads, and a deep chain of long paths would only add to the item's write cost.
	maxStoredChain = 8
)

type storableFileAccess struct {
	event  FileAccessEvent
	micros int64
	sk     string
}

// selectFileAccess returns the file access events of an upload that the events table can store, at most limit of
// them, and logs the others as dropped. earliest and latest bound access_time in Unix seconds.
func selectFileAccess(machineID string, events []FileAccessEvent, earliest, latest float64, limit int) (selected []storableFileAccess, overLimit int) {
	seen := map[string]bool{}
	var incomplete, outside int
	for _, e := range events {
		if e.AccessTime == 0 || e.Target == "" || e.Decision == "" || e.Decision == "FILE_ACCESS_DECISION_UNKNOWN" {
			incomplete++
			continue
		}
		// NaN fails both comparisons, so it is rejected here too.
		if !(e.AccessTime >= earliest && e.AccessTime <= latest) {
			outside++
			continue
		}
		micros := int64(math.Round(e.AccessTime * 1e6))
		pid := 0
		if len(e.ProcessChain) > 0 {
			pid = e.ProcessChain[0].ProcessID
		}
		// The hash of the uncut fields tells apart two accesses by one process at one instant (a process opening
		// several protected files), while the same event uploaded again keeps its key and is stored once.
		h := sha256.New()
		fmt.Fprintf(h, "%s\x00%s\x00%s", e.RuleName, e.Target, e.Decision)
		sk := fmt.Sprintf("%020d#%d#%s", micros, pid, hex.EncodeToString(h.Sum(nil))[:16])
		if seen[sk] {
			continue
		}
		if len(selected) >= limit {
			overLimit++
			continue
		}
		seen[sk] = true
		selected = append(selected, storableFileAccess{event: e, micros: micros, sk: sk})
	}
	logDropped(machineID, incomplete, "file access event without an access time, a target or a known decision")
	logDropped(machineID, outside, "file access time more than a day ahead or older than the 90 days kept")
	return selected, overLimit
}

// fileAccessItem builds the item of a file access event. Strings are cut for the same reasons as an execution's (see
// cutFields), and empty attributes are left out, as Decision and Fleet are index keys.
func fileAccessItem(machineID string, c storableFileAccess, expiry string) map[string]types.AttributeValue {
	e := c.event
	item := map[string]types.AttributeValue{
		"PK":           str(fileAccessPKPrefix + machineID),
		"SK":           str(c.sk),
		"MachineID":    str(machineID),
		"TS":           str(fmt.Sprintf("%020d", c.micros)),
		"Fleet":        str(fileAccessFleet),
		"Decision":     str(cut(e.Decision, maxIndexKeyBytes)),
		"ExpiresAfter": &types.AttributeValueMemberN{Value: expiry},
	}
	attrs := map[string]string{
		"RuleName":    e.RuleName,
		"RuleVersion": e.RuleVersion,
		"Target":      e.Target,
	}
	if len(e.ProcessChain) > 0 {
		p := e.ProcessChain[0]
		attrs["ProcessPath"] = p.FilePath
		attrs["ProcessName"] = p.FilePath[strings.LastIndex(p.FilePath, "/")+1:]
		attrs["ProcessSHA256"] = p.FileSHA256
		attrs["ProcessSigningID"] = p.SigningID
		attrs["ProcessTeamID"] = p.TeamID
		attrs["ProcessCDHash"] = p.CDHash
		if p.ProcessID != 0 {
			item["PID"] = &types.AttributeValueMemberN{Value: strconv.Itoa(p.ProcessID)}
		}
		chain := make([]types.AttributeValue, 0, min(len(e.ProcessChain), maxStoredChain))
		for _, q := range e.ProcessChain[:min(len(e.ProcessChain), maxStoredChain)] {
			chain = append(chain, str(cut(q.FilePath, maxStoredStringBytes)))
		}
		item["ProcessChain"] = &types.AttributeValueMemberL{Value: chain}
	}
	if len(e.ProcessChain) > 1 {
		attrs["ParentPath"] = e.ProcessChain[1].FilePath
	}
	for name, value := range attrs {
		if value = cut(value, maxStoredStringBytes); value != "" {
			item[name] = str(value)
		}
	}
	return item
}

// putFileAccess writes one file access event unless it is already stored. It reports whether this call wrote it,
// and whether DynamoDB refused it as invalid: such an event is dropped, as a retry would be refused the same way and
// failing the upload would block every later event of the Mac (Santa re-sends the failed batch first).
func (s *eventStore) putFileAccess(ctx context.Context, machineID string, c storableFileAccess, expiry string) (written, invalid bool, err error) {
	_, err = s.api.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		Item:                fileAccessItem(machineID, c, expiry),
		ConditionExpression: aws.String("attribute_not_exists(PK)"),
	})
	var conditional *types.ConditionalCheckFailedException
	var apiErr smithy.APIError
	switch {
	case err == nil:
		return true, false, nil
	case errors.As(err, &conditional), errors.As(err, &apiErr) && apiErr.ErrorCode() == "ConditionalCheckFailedException":
		// Stored by an earlier upload of the same batch.
		return false, false, nil
	case errors.As(err, &apiErr) && apiErr.ErrorCode() == "ValidationException":
		return false, true, nil
	default:
		return false, false, err
	}
}
