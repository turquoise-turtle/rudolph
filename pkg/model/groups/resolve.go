package groups

import (
	"sort"

	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
)

// strength orders the policies a group rule can have, strongest first. A conditional block (CEL) beats an
// unconditional allow, so that a group cannot loosen what another group of the same Mac restricts.
var strength = map[types.Policy]int{
	types.RulePolicyBlocklist:          7,
	types.RulePolicySilentTTYBlocklist: 6,
	types.RulePolicySilentGUIBlocklist: 5,
	types.RulePolicySilentBlocklist:    4,
	types.RulePolicyCEL:                3,
	types.RulePolicyAllowlist:          2,
	types.RulePolicyAllowlistCompiler:  1,
}

// Applicable reports whether a stored group rule can be sent to Santa. Rows that fail are skipped (rows are written by
// the CLI, which refuses them, but could be edited by hand).
func Applicable(rule rules.SantaRule) bool {
	if _, ok := strength[rule.Policy]; !ok {
		return false
	}
	return rules.ValidatePolicy(rule) == nil
}

// GroupRules is a group's rules, as Resolve takes them
type GroupRules struct {
	Group string
	Rules []RuleRow
}

// Resolve returns, for each rule sort key that one of the groups has a rule for, the rule to send: the one with the
// strongest policy, and between equal policies the one of the group whose name sorts first. Keys in skip (covered by
// a machine rule, which beats any group rule) are left out.
func Resolve(groupRules []GroupRules, skip func(sortKey string) bool) map[string]RuleRow {
	sorted := append([]GroupRules(nil), groupRules...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Group < sorted[j].Group })

	winners := map[string]RuleRow{}
	for _, g := range sorted {
		for _, row := range g.Rules {
			if !Applicable(row.SantaRule) || skip(row.SortKey) {
				continue
			}
			if ruleType, _, ok := rules.RuleTypeIdentifierFromSortKey(row.SortKey); !ok || ruleType != row.RuleType {
				continue
			}
			// Strictly stronger only: groups are visited in name order, so the first group keeps a tie
			if current, ok := winners[row.SortKey]; !ok || strength[row.Policy] > strength[current.Policy] {
				winners[row.SortKey] = row
			}
		}
	}
	return winners
}
