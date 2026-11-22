package deployments

// Collection of allowed executions for group rules (owner approved 2026-11-23). To decide whether a group rule's
// allowed execution is stored, eventupload now also reads, from the store table, the group registry (PK
// "GroupRegistry"), the uploading Mac's groups item (PK "Machine#<id>", SK "Groups") and group rule rows
// ("GroupRules#<group>"), besides the collection index ("EventCollection") and the global and machine rule rows it
// read before. It takes a stale group rule out of the index in a transaction that checks the group rule row, so its
// ConditionCheckItem covers "GroupRules#*" too. The UpdateItem stays limited to the index item, and eventupload gets no
// other action on the store table: in particular it never writes group rules, the registry or a Mac's partition.
//
// Every ForAllValues LeadingKeys condition keeps a Null check beside it (TestIAMReview_LeadingKeysConditionsRequireTheKey).

import (
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// eventsStoreGrant is one events_write statement on the store table: its actions and its LeadingKeys values.
type eventsStoreGrant struct {
	actions []string
	keys    []string
	test    string
	null    bool
}

func eventsWriteStoreStatements(t *testing.T) []eventsStoreGrant {
	t.Helper()
	docs, files := policyDocs(t, santaAPI+"/modules/store")
	doc := docs["events_write"]
	if doc == nil {
		t.Fatal("no events_write policy document")
	}
	var out []eventsStoreGrant
	for _, st := range statementsOf(doc) {
		text := squash(blockText(t, files, st))
		if !strings.Contains(text, "aws_dynamodb_table.store.arn") {
			continue
		}
		s := eventsStoreGrant{}
		if attr := st.Body.Attributes["actions"]; attr != nil {
			val, diags := attr.Expr.Value(nil)
			if diags.HasErrors() {
				t.Fatal(diags.Error())
			}
			for it := val.ElementIterator(); it.Next(); {
				_, e := it.Element()
				s.actions = append(s.actions, e.AsString())
			}
		}
		for _, cond := range nested(st, "condition") {
			variable, _ := literal(cond.Body.Attributes["variable"])
			test, _ := literal(cond.Body.Attributes["test"])
			if variable != "dynamodb:LeadingKeys" {
				continue
			}
			if test == "Null" {
				s.null = strings.Contains(squash(blockText(t, files, cond)), `"false"`)
				continue
			}
			s.test = test
			s.keys = append(s.keys, eventsLeadingKeyValues(t, cond)...)
		}
		sort.Strings(s.actions)
		sort.Strings(s.keys)
		out = append(out, s)
	}
	return out
}

func eventsLeadingKeyValues(t *testing.T, cond *hclsyntax.Block) []string {
	t.Helper()
	val, diags := cond.Body.Attributes["values"].Expr.Value(nil)
	if diags.HasErrors() {
		t.Fatal(diags.Error())
	}
	var keys []string
	for it := val.ElementIterator(); it.Next(); {
		_, e := it.Element()
		keys = append(keys, e.AsString())
	}
	return keys
}

// eventsStoreGrantFor returns the LeadingKeys under which events_write grants action on the store table, failing the test
// when no statement or more than one grants it.
func eventsStoreGrantFor(t *testing.T, statements []eventsStoreGrant, action string) eventsStoreGrant {
	t.Helper()
	var found []eventsStoreGrant
	for _, s := range statements {
		for _, a := range s.actions {
			if a == action {
				found = append(found, s)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d events_write statements grant %s on the store table, want 1", len(found), action)
	}
	return found[0]
}

func TestIAM_EventuploadReadsWhatGroupCollectionNeeds(t *testing.T) {
	statements := eventsWriteStoreStatements(t)
	for action, want := range map[string][]string{
		"dynamodb:GetItem":            {"EventCollection", "GlobalRules", "GroupRegistry", "GroupRules#*", "Machine#*", "MachineRules#*"},
		"dynamodb:ConditionCheckItem": {"GlobalRules", "GroupRules#*", "MachineRules#*"},
		"dynamodb:UpdateItem":         {"EventCollection"},
	} {
		s := eventsStoreGrantFor(t, statements, action)
		if strings.Join(s.keys, ",") != strings.Join(want, ",") {
			t.Errorf("events_write grants %s on the store table for LeadingKeys %v, want %v", action, s.keys, want)
		}
		if !strings.HasPrefix(s.test, "ForAllValues:") {
			t.Errorf("the %s LeadingKeys condition is %q, want a ForAllValues test", action, s.test)
		}
		for _, k := range s.keys {
			if strings.Contains(k, "*") && !strings.HasSuffix(s.test, "StringLike") {
				t.Errorf("the %s condition holds the pattern %q under %q, which matches it literally", action, k, s.test)
			}
		}
		if !s.null {
			t.Errorf("the %s LeadingKeys condition has no Null check beside it", action)
		}
	}
}

func TestIAM_EventuploadHasNoOtherStoreAction(t *testing.T) {
	allowed := map[string]bool{"dynamodb:GetItem": true, "dynamodb:ConditionCheckItem": true, "dynamodb:UpdateItem": true}
	for _, s := range eventsWriteStoreStatements(t) {
		for _, a := range s.actions {
			if !allowed[a] {
				t.Errorf("events_write grants %s on the store table", a)
			}
		}
	}
}
