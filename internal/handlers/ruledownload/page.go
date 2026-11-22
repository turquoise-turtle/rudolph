package ruledownload

import (
	"encoding/json"
	"time"

	"github.com/airbnb/rudolph/pkg/model/rules"
)

// defaultResponseBudgetBytes is how many bytes of rules one ruledownload response carries, as they count against
// Lambda's 6 MB response limit (see encodedRuleBytes).
//
// It is a third of that limit, not most of it, for three reasons. A rule written before the field limits of
// pkg/model/rules/field_limits.go can be as large as a DynamoDB item (400 KB), and a page always carries at least one
// rule. Santa gives a request 30 seconds in all, download included, and API Gateway does not compress responses, so a
// page must get through a slow link in that time. And a larger page means more 1 MB queries in one invocation of a
// Lambda that times out after 10 seconds.
const defaultResponseBudgetBytes = 2 * 1024 * 1024

// pageReadTime is how long a global rules page keeps sending queries. Filling a page takes several (DynamoDB returns
// at most 1 MB per query); when they are slow (throttled and retried, say), the page is sent with what it has rather
// than lost to the Lambda timeout with everything it read.
const pageReadTime = 3 * time.Second

// encodedRuleBytes returns how many bytes the rule adds to the Lambda proxy response. The rules are JSON inside the
// response's body, which is itself a JSON string in the proxy response Lambda serialises, so every quote and
// backslash of the rule's JSON takes two bytes there. Nothing else grows: encoding/json has already escaped whatever
// a JSON string escapes (control characters, <, > and &, invalid UTF-8) in the rule's own JSON. The count includes
// the comma between rules.
func encodedRuleBytes(rule RuledownloadRule) int {
	encoded, err := json.Marshal(rule)
	if err != nil {
		// The response cannot be serialised either, and that failure is the one reported
		return 0
	}
	size := len(encoded) + 1
	for _, b := range encoded {
		if b == '"' || b == '\\' {
			size++
		}
	}
	return size
}

// rulePage collects the rules of one response, within the response budget.
type rulePage struct {
	silentBlocklistOnly bool
	budget              int
	rules               []RuledownloadRule
	bytes               int
}

func newRulePage(budget int, silentBlocklistOnly bool) *rulePage {
	return &rulePage{budget: budget, silentBlocklistOnly: silentBlocklistOnly, rules: []RuledownloadRule{}}
}

// add appends the rule as the sync's client is sent it, and reports false, adding nothing, when the page has no room
// left for it. A page always takes its first rule, whatever its size: a rule that fits no page would otherwise stop
// every sync at the same place. A rule that is never sent (see responseRuleForClient) takes no room.
func (p *rulePage) add(rule rules.SantaRule) bool {
	responseRule, sent := responseRuleForClient(rule, p.silentBlocklistOnly)
	if !sent {
		return true
	}
	size := encodedRuleBytes(responseRule)
	if len(p.rules) > 0 && p.bytes+size > p.budget {
		return false
	}
	p.rules = append(p.rules, responseRule)
	p.bytes += size
	return true
}

// queryLimit returns how many more rules to ask DynamoDB for, judging by the size of those the page holds: 0 for no
// limit while the page is empty, and -1 when the page is as good as full. Asking for no more than are likely to fit
// keeps the rows read and then left for the next page, which are paid for twice, to a few.
func (p *rulePage) queryLimit() int {
	if len(p.rules) == 0 {
		return 0
	}
	// A page within a sixteenth of its budget is sent as it is, rather than topped up by ever smaller queries
	remaining := p.budget - p.budget/16 - p.bytes
	if remaining <= 0 {
		return -1
	}
	average := p.bytes/len(p.rules) + 1
	return remaining/average + 1
}
