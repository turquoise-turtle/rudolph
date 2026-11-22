package ruledownload

import (
	"errors"
	"fmt"
	"log"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/feedrules"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/model/syncstate"
)

// ruledownloadCursor is passed between the server and client between successive API calls to /ruledownload
// This object not only represents a pagination cursor, but is also a way for the server and client to exchange
// metadata about the sync process without having to resort to serverside storage.
type ruledownloadCursor struct {
	// The Strategy determines HOW the server handles the
	Strategy ruledownloadStrategy `json:"strategy,omitempty"`
	ruledownloadCursorDDBLastEvaluatedKey
	PageNumber int `json:"page,omitempty"`
	// BatchSize is the sync's event upload batch size. It no longer sizes a page (see rulePage); it stays in the cursor,
	// and is checked like before, so that cursors keep the shape the previous version issues and accepts.
	BatchSize int `json:"batch_size,omitempty"`
	// SyncSeq is the sync (syncstate.SyncState.SyncSeq) whose first page issued the cursor, so that the last page of
	// an older sync, served after a newer sync of the machine started, is not recorded as the newer sync's. Absent in
	// cursors issued by earlier versions of Rudolph.
	SyncSeq int `json:"seq,omitempty"`
	// SilentBlocklistOnly carries syncstate.SyncState.SilentBlocklistOnly of the sync to its later pages, which do not
	// read the sync state: the client predates the silent GUI and TTY policies.
	SilentBlocklistOnly bool `json:"silent_only,omitempty"`
	// TailSection and TailSortKey are only set once the sync's global or feed rules have all been sent and its
	// machine-specific rules did not fit on that page: the section of the tail the next page goes on with (see
	// machineTail), and the sort key of the last rule of that section already sent ("" for none). A page with such a
	// cursor reads no global or feed rules. Its key is then not where to read on from, but what the last page records
	// about the feed: the last feed row the sync served, or no key when it served none (see tailFeedServedUpTo).
	TailSection int    `json:"tail,omitempty"`
	TailSortKey string `json:"tail_sk,omitempty"`

	// syncState is the sync state read for the first page of a sync; it is never sent to the client. When the first
	// page is also the last one (e.g. a steady-state incremental sync), the machine rules page reuses it instead of
	// reading the sync state again.
	syncState *syncstate.SyncStateRow
}

type ruledownloadCursorDDBLastEvaluatedKey struct {
	PartitionKey string `json:"pk,omitempty"`
	SortKey      string `json:"sk,omitempty"`
}

func (r *ruledownloadCursor) SetDynamodbLastEvaluatedKey(pk *dynamodb.PrimaryKey) {
	r.ruledownloadCursorDDBLastEvaluatedKey = ruledownloadCursorDDBLastEvaluatedKey(*pk)
}

// ToDynamoDBPrimaryKey converts the cursor into a DDB Primary Key
// When the cursor does not actually contain any values for the partition or sort key,
// returns nil to indicate that it does not have a last evaluated key
func (r ruledownloadCursor) GetLastEvaluatedKey() *dynamodb.PrimaryKey {
	if r.PartitionKey != "" || r.SortKey != "" {
		return (*dynamodb.PrimaryKey)(&r.ruledownloadCursorDDBLastEvaluatedKey)
	}
	return nil
}

// onTail reports whether the cursor asks for a later page of the machine-specific rules (see TailSection)
func (r ruledownloadCursor) onTail() bool {
	return r.TailSection != 0
}

func (r ruledownloadCursor) tailPosition() tailPosition {
	return tailPosition{section: r.TailSection, sortKey: r.TailSortKey}
}

func (r ruledownloadCursor) CloneForNextPage() ruledownloadCursor {
	return ruledownloadCursor{
		Strategy:   r.Strategy,
		BatchSize:  r.BatchSize,
		PageNumber: r.PageNumber + 1,
		SyncSeq:    r.SyncSeq,

		SilentBlocklistOnly: r.SilentBlocklistOnly,
	}
}

// maxSortKeyBytes is DynamoDB's limit on a sort key.
const maxSortKeyBytes = 1024

// maxPageNumber bounds the page of a global or feed cursor, far beyond any real fleet. A cursor at this page would have
// the server issue one for page 1,000,001, which is refused; that needs a rule set of over a million pages, so it is
// unreachable in practice.
const maxPageNumber = 1_000_000

// validate refuses a cursor the server could never have issued. The cursor comes back from the client as it was sent,
// so it is client input: without these checks a forged one reached DynamoDB and was answered 500, which the api_5xx
// alarm counts as a server failure and Santa retries. Machine pages read neither the batch size nor the key, and are
// only first requested with cursors from earlier versions of Rudolph, which issued them with any page or none, so only
// their strategy, the sign of their page and seq, and their place in the tail are checked.
func (r ruledownloadCursor) validate() error {
	// Pages count from 1 and seq is the preflight time in milliseconds, so neither is ever negative.
	if r.PageNumber < 0 || r.SyncSeq < 0 {
		return fmt.Errorf("negative page %d or seq %d", r.PageNumber, r.SyncSeq)
	}
	if r.TailSection != 0 || r.TailSortKey != "" {
		// The place in the tail is only compared with the sort keys of the machine's own rules, never sent to
		// DynamoDB. It is held to what Rudolph can issue: one of the sections, and a sort key no longer than DynamoDB
		// stores. The key's form is not checked: it is that of a stored row, and a row whose key is not a rule's
		// would otherwise stop every sync at the page after it.
		if r.TailSection < tailMachineRules || r.TailSection > tailReplacements {
			return fmt.Errorf("tail section %d outside %d-%d", r.TailSection, tailMachineRules, tailReplacements)
		}
		if len(r.TailSortKey) > maxSortKeyBytes {
			return fmt.Errorf("the tail sort key is longer than %d bytes", maxSortKeyBytes)
		}
	}
	var partition string
	switch r.Strategy {
	case ruledownloadStrategyClean:
		partition = globalrules.Partition
	case ruledownloadStrategyIncremental:
		partition = feedrules.Partition
	case ruledownloadStrategyMachine:
		return nil
	default:
		return fmt.Errorf("unknown strategy %d", r.Strategy)
	}
	// A global or feed cursor is only issued for the page after the first, and no fleet has a million pages; a page in
	// the quintillions would also overflow when the next page's cursor is made
	if r.PageNumber < 2 || r.PageNumber > maxPageNumber {
		return fmt.Errorf("page %d outside 2-%d", r.PageNumber, maxPageNumber)
	}
	if r.BatchSize < machineconfiguration.MinBatchSize || r.BatchSize > machineconfiguration.MaxBatchSize {
		return fmt.Errorf("batch size %d outside %d-%d", r.BatchSize, machineconfiguration.MinBatchSize, machineconfiguration.MaxBatchSize)
	}
	// Rudolph only issues a global or feed cursor for a later page, with the whole key DynamoDB returned after the last
	// row of the page before; DynamoDB refuses a start key with an empty part or a sort key over 1024 bytes. A cursor
	// on the tail is the one exception: it has no key at all when the sync served no feed row (a clean sync never
	// carries one there).
	if r.onTail() && r.PartitionKey == "" && r.SortKey == "" {
		return nil
	}
	if r.PartitionKey != partition {
		return errors.New("the cursor's key is not in the partition its strategy reads")
	}
	if r.SortKey == "" || len(r.SortKey) > maxSortKeyBytes {
		return fmt.Errorf("the cursor's sort key is empty or longer than %d bytes", maxSortKeyBytes)
	}
	return nil
}

func (r *ruledownloadCursor) SetStrategy(strategy ruledownloadStrategy) {
	r.Strategy = strategy
}

type ruledownloadStrategy int

const (
	// The clean download strategy downloads from the GlobalRules
	// The LastEvaluatedKey references the sort key where pk = "GlobalRules"
	// All clean downloads will paginate over all global rules
	ruledownloadStrategyClean ruledownloadStrategy = iota + 1

	// The incremental download strategy downloads from the rules feed
	// The LastEvaluatedKey references the sort key where pk = "FeedRules"
	// Incremental downloads will read the last incremental download cursor from the previous
	// sync state in the database and paginate from there, never re-reading rules that have already
	// been read from the feed.
	ruledownloadStrategyIncremental

	// The machine download strategy: a page of machine-specific rules only, for a sync whose first cursor was issued
	// by an earlier version of Rudolph. The LastEvaluatedKey is ignored; TailSection and TailSortKey say where a later
	// page goes on.
	ruledownloadStrategyMachine
)

type ruledownloadCursorService interface {
	ConstructCursor(ruledownloadRequest RuledownloadRequest, machineID string) (cursor ruledownloadCursor, err error)
}

type concreteRuledownloadCursorService struct {
	// timer and updater are unused since ruledownload stopped recording on the sync state when it started (a write
	// per sync that nothing read; see C3 in docs/audit/shortcomings.md). The unit tests still set them.
	timer   clock.TimeProvider
	updater dynamodb.UpdateItemAPI
	getter  dynamodb.GetItemAPI
}

// ConstructCursor takes the given ruledownload request and constructs a cursor object. This cursor is sent to
// the ruledownload services for processing.
//
// In general, if a cursor already exists in the request, it will be accepted. Else, the cursor is generated anew,
// depending on what the sync state is in the database.
func (c concreteRuledownloadCursorService) ConstructCursor(ruledownloadRequest RuledownloadRequest, machineID string) (cursor ruledownloadCursor, err error) {
	if ruledownloadRequest.Cursor == nil {
		// With a nil cursor, this is the very first ruledownload request in a sync.
		// Couple things we need to do:
		//  * determine the ruledownload strategy
		//  * embed any context, including the desired strategy, into the next cursor
		log.Printf("  Ruledownload first page")

		// Get the sync state as set by /preflight. The sync state will contain the final result of the previous
		syncState, eerr := syncstate.GetByMachineID(c.getter, machineID)
		if eerr != nil {
			err = fmt.Errorf("failed to get previous syncstate: %w", eerr)
			return
		}
		if syncState == nil {
			err = errNoSyncState
			return
		}

		if syncState.CleanSync {
			// Clean syncs never start with a LastEvaluatedKey as it always begins from the beginning
			cursor = ruledownloadCursor{
				Strategy:   ruledownloadStrategyClean,
				BatchSize:  syncState.BatchSize,
				PageNumber: 1,
			}
		} else {
			// Clean syncs
			lastEvalKey := feedrules.ReconstructFeedSyncLastEvaluatedKeyFromDate(syncState.FeedSyncCursor)
			cursor = ruledownloadCursor{
				Strategy:                              ruledownloadStrategyIncremental,
				ruledownloadCursorDDBLastEvaluatedKey: ruledownloadCursorDDBLastEvaluatedKey(lastEvalKey),
				BatchSize:                             syncState.BatchSize,
				PageNumber:                            1,
			}
		}

		cursor.syncState = syncState
		cursor.SyncSeq = syncState.SyncSeq
		cursor.SilentBlocklistOnly = syncState.SilentBlocklistOnly

	} else {
		cursor = *ruledownloadRequest.Cursor
	}
	return
}
