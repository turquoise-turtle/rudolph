package ruledownload

// Pages sized by response size (fix/scale): the measure of a rule (encodedRuleBytes), pages that always make
// progress, a global page cut in the middle of a query, the cursor checks on the paged machine-specific tail, and the
// tail's order across its three sections. Tail paging against a real table is in scale_paging_integration_test.go.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/response"
	rudolphtypes "github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// lambdaSerialised is the proxy response as the Lambda runtime writes it (aws-lambda-go's handler encodes with
// SetEscapeHTML(false), and Encode appends a newline).
func lambdaSerialised(t *testing.T, resp *events.APIGatewayProxyResponse) int {
	t.Helper()
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(resp); err != nil {
		t.Fatal(err)
	}
	return out.Len()
}

// fillTo repeats fill as many whole times as fit in limit bytes.
func fillTo(fill string, limit int) string {
	return strings.Repeat(fill, limit/len(fill))
}

// encodedRuleBytes must be exactly what a rule adds to the serialised Lambda proxy response, for every kind of
// character that the rule's JSON or the body string escapes differently, with every free-form field at its limit. An
// undercount lets a page past the budget; the budget is a third of Lambda's 6 MB so a small error would not show
// until a page of escaped rules, which this pins.
func TestScale_EncodedRuleBytesIsWhatTheRuleAddsToTheLambdaResponse(t *testing.T) {
	fills := map[string]string{
		"quote":             `"`,
		"backslash":         `\`,
		"control 0x01":      "\x01",
		"less than":         "<",
		"invalid byte 0xff": "\xff",
		"U+2028":            "\u2028",
		"non-ASCII letter":  "\u00e9",
		"a":                 "a",
	}
	base := RuledownloadRule{RuleType: rudolphtypes.RuleTypeTeamID, Policy: rudolphtypes.RulePolicyAllowlist, Identifier: "EQHXZ8M8AV"}
	worst := 0
	for name, fill := range fills {
		t.Run(name, func(t *testing.T) {
			if name == "U+2028" || name == "non-ASCII letter" {
				if utf8.RuneCountInString(fill) != 1 || len(fill) < 2 {
					t.Fatalf("the fill %q is not one multi-byte character", fill)
				}
			}
			rule := RuledownloadRule{
				RuleType:               rudolphtypes.RuleTypeBinary,
				Policy:                 rudolphtypes.RulePolicyCEL,
				Identifier:             strings.Repeat("ab", 32),
				CustomMessage:          fillTo(fill, rules.MaxCustomMessageBytes),
				CustomURL:              fillTo(fill, rules.MaxCustomURLBytes),
				NotificationAppName:    fillTo(fill, rules.MaxNotificationAppNameBytes),
				CELExpr:                fillTo(fill, rules.MaxCELExprBytes),
				EventDetailButtonLabel: fillTo(fill, rules.MaxEventDetailButtonLabelBytes),
			}
			for _, cursor := range []string{"", `{"strategy":1,"page":2}`} {
				without, err := response.APIResponse(200, RuledownloadResponse{Rules: []RuledownloadRule{base}, Cursor: cursor})
				if err != nil {
					t.Fatal(err)
				}
				with, err := response.APIResponse(200, RuledownloadResponse{Rules: []RuledownloadRule{base, rule}, Cursor: cursor})
				if err != nil {
					t.Fatal(err)
				}
				grew := lambdaSerialised(t, with) - lambdaSerialised(t, without)
				if got := encodedRuleBytes(rule); got != grew {
					t.Errorf("encodedRuleBytes says %d bytes, but adding the rule grew the Lambda response by %d", got, grew)
				}
				// fakesanta measures with json.Marshal (HTML escaping on); it must agree
				withMarshal, _ := json.Marshal(with)
				withoutMarshal, _ := json.Marshal(without)
				if got := encodedRuleBytes(rule); got != len(withMarshal)-len(withoutMarshal) {
					t.Errorf("encodedRuleBytes says %d bytes, but json.Marshal of the response grew by %d", got, len(withMarshal)-len(withoutMarshal))
				}
			}
			if size := encodedRuleBytes(rule); size > worst {
				worst = size
			}
		})
	}
	// rule-pagination.md: "about 176 KB"; a page always carries one rule, so this bounds a page of one rule
	if worst >= 180_000 {
		t.Errorf("the largest rule within the field limits is %d bytes in the response, want under 180,000", worst)
	}
	t.Logf("largest rule within the field limits: %d bytes", worst)
}

// scaleTable is r4Table (the machine's rules and tracking items, writes recorded but not applied) with global rules
// that Query pages like DynamoDB: in sort key order, from ExclusiveStartKey, at most Limit rows and, like the 1 MB
// cap, at most perQuery rows (0 for no cap), with a LastEvaluatedKey when it stopped early.
type scaleTable struct {
	*r4Table
	global   []globalrules.GlobalRuleRow
	perQuery int
	// globalQueries and globalResults are the global rule queries made and how many rows each returned
	globalQueries []*awsdynamodb.QueryInput
	globalResults []int
}

func queriesPartition(input *awsdynamodb.QueryInput, pk string) bool {
	for _, value := range input.ExpressionAttributeValues {
		if s, ok := value.(*types.AttributeValueMemberS); ok && s.Value == pk {
			return true
		}
	}
	return false
}

func (s *scaleTable) Query(input *awsdynamodb.QueryInput) (*awsdynamodb.QueryOutput, error) {
	if !queriesPartition(input, globalrules.Partition) {
		return s.r4Table.Query(input)
	}
	s.queries++
	s.globalQueries = append(s.globalQueries, input)
	start := ""
	if sk, ok := input.ExclusiveStartKey["SK"].(*types.AttributeValueMemberS); ok {
		start = sk.Value
	}
	limit := len(s.global) + 1
	if input.Limit != nil {
		limit = int(*input.Limit)
	}
	if s.perQuery > 0 && s.perQuery < limit {
		limit = s.perQuery
	}
	out := &awsdynamodb.QueryOutput{}
	for _, row := range s.global {
		if row.SortKey <= start {
			continue
		}
		if len(out.Items) == limit {
			// The key of the last row returned
			last := out.Items[len(out.Items)-1]
			out.LastEvaluatedKey = map[string]types.AttributeValue{"PK": last["PK"], "SK": last["SK"]}
			break
		}
		item, err := attributevalue.MarshalMap(row)
		if err != nil {
			s.t.Fatal(err)
		}
		out.Items = append(out.Items, item)
	}
	s.globalResults = append(s.globalResults, len(out.Items))
	return out, nil
}

func scaleHandler(table *scaleTable, budget int) *PostRuledownloadHandler {
	pages := machineRulePage{
		queryer: table, getter: table, updater: table, batchGetter: table, batchWriter: table, timer: mockTimeProvider{},
		responseBudget: budget,
	}
	return &PostRuledownloadHandler{
		booted:        true,
		cursorService: concreteRuledownloadCursorService{getter: table},
		ghandler:      concreteGlobalRuleDownloader{queryer: table, machineRules: pages},
		fhandler:      concreteFeedRuleDownloader{queryer: table, machineRules: pages},
		mhandler:      concreteMachineRuleDownloader{machineRules: pages},
	}
}

func scaleSHA(i int) string { return fmt.Sprintf("%064x", i) }

func scaleGlobalRule(i int, customMessage string) globalrules.GlobalRuleRow {
	id := scaleSHA(i)
	return globalrules.GlobalRuleRow{
		PrimaryKey: dynamodb.PrimaryKey{PartitionKey: globalrules.Partition, SortKey: rules.RuleSortKeyFromTypeIdentifier(id, rudolphtypes.RuleTypeBinary)},
		SantaRule:  rules.SantaRule{RuleType: rudolphtypes.RuleTypeBinary, Policy: rudolphtypes.RulePolicyBlocklist, Identifier: id, CustomMessage: customMessage},
	}
}

func scaleMachineRule(i int, policy rudolphtypes.Policy, customMessage string) machinerules.MachineRuleRow {
	id := scaleSHA(i)
	return machinerules.MachineRuleRow{
		PrimaryKey: dynamodb.PrimaryKey{PartitionKey: machinerules.MachineRulesPK(inputHandlingMachineID), SortKey: rules.RuleSortKeyFromTypeIdentifier(id, rudolphtypes.RuleTypeBinary)},
		SantaRule:  rules.SantaRule{RuleType: rudolphtypes.RuleTypeBinary, Policy: policy, Identifier: id, CustomMessage: customMessage},
	}
}

// scalePage is one response of a sync followed through Handle.
type scalePage struct {
	rules     []RuledownloadRule
	rawCursor string
	cursor    ruledownloadCursor
}

func (p scalePage) identifiers() []string {
	out := make([]string, len(p.rules))
	for i, rule := range p.rules {
		out[i] = rule.Identifier
	}
	return out
}

// followSync sends body and then every cursor the server issues, as Santa does, and returns the pages. Every response
// must be 200 and every cursor must pass validate.
func followSync(t *testing.T, h *PostRuledownloadHandler, body string) []scalePage {
	t.Helper()
	var pages []scalePage
	for {
		resp := r2Handle(t, h, body)
		if resp.StatusCode != 200 {
			t.Fatalf("page %d (request %s) was answered %d: %s", len(pages)+1, body, resp.StatusCode, resp.Body)
		}
		var page RuledownloadResponse
		if err := json.Unmarshal([]byte(resp.Body), &page); err != nil {
			t.Fatal(err)
		}
		p := scalePage{rules: page.Rules, rawCursor: page.Cursor}
		if page.Cursor != "" {
			if err := json.Unmarshal([]byte(page.Cursor), &p.cursor); err != nil {
				t.Fatalf("page %d issued a cursor that does not parse: %s", len(pages)+1, page.Cursor)
			}
			if err := p.cursor.validate(); err != nil {
				t.Fatalf("page %d issued cursor %s, which validate refuses: %v", len(pages)+1, page.Cursor, err)
			}
		}
		pages = append(pages, p)
		if page.Cursor == "" {
			return pages
		}
		if len(pages) > 100 {
			t.Fatal("more than 100 pages: the cursor does not move")
		}
		body = r2Body(t, page.Cursor)
	}
}

// A page always takes its first rule, so a rule larger than the whole budget is sent alone and the cursor moves past
// it, for a global rule (the key) and for a machine rule (tail_sk); otherwise every sync would stop at that rule.
func TestScale_ARuleLargerThanTheBudgetIsSentAloneAndPassed(t *testing.T) {
	const budget = 400
	big := strings.Repeat("m", 1000)
	state := r2SyncState()
	state.CleanSync = true
	table := &scaleTable{
		r4Table: &r4Table{r2Table: &r2Table{t: t, syncState: state}, deliveries: map[string]machinerules.Delivery{}},
		global:  []globalrules.GlobalRuleRow{scaleGlobalRule(1, ""), scaleGlobalRule(2, big), scaleGlobalRule(3, "")},
	}
	table.machineRules = []machinerules.MachineRuleRow{
		scaleMachineRule(11, rudolphtypes.RulePolicyAllowlist, ""),
		scaleMachineRule(12, rudolphtypes.RulePolicyAllowlist, big),
		scaleMachineRule(13, rudolphtypes.RulePolicyAllowlist, ""),
	}
	if encodedRuleBytes(RuledownloadRule(table.global[1].SantaRule)) <= budget {
		t.Fatal("the big rule fits the budget, so the fixture tests nothing")
	}
	pages := followSync(t, scaleHandler(table, budget), `{}`)
	want := [][]string{{scaleSHA(1)}, {scaleSHA(2)}, {scaleSHA(3), scaleSHA(11)}, {scaleSHA(12)}, {scaleSHA(13)}}
	var got [][]string
	for _, p := range pages {
		got = append(got, p.identifiers())
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("pages were\n%v\nwant\n%v", got, want)
	}
	if pages[1].cursor.SortKey != table.global[1].SortKey || pages[1].cursor.onTail() {
		t.Errorf("after the oversized global rule the cursor is %s, want its key", pages[1].rawCursor)
	}
	if pages[3].cursor.TailSection != tailMachineRules || pages[3].cursor.TailSortKey != table.machineRules[1].SortKey {
		t.Errorf("after the oversized machine rule the cursor is %s, want tail 1 at its key", pages[3].rawCursor)
	}
}

// A global page whose queries return more than fits (DynamoDB returns up to 1 MB a query, and later queries are
// limited by an estimate) is cut at the last rule that fitted, even in the middle of the second query: the next cursor
// starts right after it, so nothing read and left out is skipped. The second query asks for fewer rows than the first
// returned, judging by the size of those already on the page.
func TestScale_GlobalPageCutMidQueryResumesAfterTheLastRuleThatFitted(t *testing.T) {
	const budget = 1000
	big := strings.Repeat("m", 400)
	state := r2SyncState()
	state.CleanSync = true
	table := &scaleTable{
		r4Table:  &r4Table{r2Table: &r2Table{t: t, syncState: state}, deliveries: map[string]machinerules.Delivery{}},
		perQuery: 5,
	}
	for i := 1; i <= 6; i++ {
		table.global = append(table.global, scaleGlobalRule(i, ""))
	}
	table.global = append(table.global, scaleGlobalRule(7, big), scaleGlobalRule(8, ""))

	pages := followSync(t, scaleHandler(table, budget), `{}`)
	if len(table.globalResults) < 2 || table.globalResults[0] != 5 {
		t.Fatalf("the first page's queries returned %v rows, want 5 then more, so the fixture tests nothing", table.globalResults)
	}
	second := table.globalQueries[1]
	if second.Limit == nil || int(*second.Limit) >= table.globalResults[0] || *second.Limit < 1 {
		t.Errorf("the second query's Limit is %v, want set, at least 1 and below the %d rows the first returned", second.Limit, table.globalResults[0])
	}
	if got := pages[0].identifiers(); fmt.Sprint(got) != fmt.Sprint([]string{scaleSHA(1), scaleSHA(2), scaleSHA(3), scaleSHA(4), scaleSHA(5), scaleSHA(6)}) {
		t.Errorf("the first page holds %v, want rules 1 to 6 (rule 7 does not fit)", got)
	}
	if pages[0].cursor.SortKey != table.global[5].SortKey || pages[0].cursor.PartitionKey != globalrules.Partition {
		t.Errorf("the first page's cursor is %s, want the key of rule 6, the last that fitted", pages[0].rawCursor)
	}
	var all []string
	for _, p := range pages {
		all = append(all, p.identifiers()...)
	}
	var want []string
	for _, row := range table.global {
		want = append(want, row.Identifier)
	}
	if fmt.Sprint(all) != fmt.Sprint(want) {
		t.Errorf("the sync sent %v, want every global rule once, in order: %v", all, want)
	}
	for i, p := range pages {
		size := 0
		for _, rule := range p.rules {
			size += encodedRuleBytes(rule)
		}
		if size > budget && len(p.rules) > 1 {
			t.Errorf("page %d holds %d bytes of rules in %d rules, over the %d byte budget", i+1, size, len(p.rules), budget)
		}
	}
}

// scaleTailCursors are cursors of later pages of the machine-specific tail, one per strategy and key shape, for the
// sync numbered seq ("" leaves seq out).
func scaleTailCursors(seq string) map[string]string {
	if seq != "" {
		seq = `,"seq":` + seq
	}
	return map[string]string{
		"clean sync, tail 1":                `{"strategy":1,"batch_size":50,"page":3,"tail":1,"tail_sk":"Binary#aa"` + seq + `}`,
		"feed sync with a key, tail 2":      `{"strategy":2,"batch_size":50,"page":3,"pk":"RulesFeed","sk":"2000-01-01T00:00:00Z#Binary#aa","tail":2` + seq + `}`,
		"feed sync without a key, tail 3":   `{"strategy":2,"batch_size":50,"page":3,"tail":3,"tail_sk":"Binary#aa"` + seq + `}`,
		"machine page (strategy 3), tail 1": `{"strategy":3,"page":3,"tail":1,"tail_sk":"Binary#aa"` + seq + `}`,
	}
}

// Every tail page builds the tail again and may write tracking and record the last page, so it is refused like a last
// page: a superseded sync's (stale seq) with 409, one without a sync state with 400, and a strategy 1 or 2 one
// without seq for a numbered sync with 409. Strategy 3 without seq is the documented exemption, and the current
// sync's is served.
func TestScale_TailPagesAreRefusedLikeALastPage(t *testing.T) {
	current := fmt.Sprint(r2CurrentSync)
	for name, cursor := range scaleTailCursors(fmt.Sprint(r2CurrentSync - 1)) {
		t.Run("stale seq, "+name, func(t *testing.T) {
			resp := r2Handle(t, r2Handler(&r2Table{t: t, syncState: r2SyncState()}), r2Body(t, cursor))
			r2WantError(t, resp, 409, "a tail page of a superseded sync ("+cursor+")")
		})
	}
	for name, cursor := range scaleTailCursors(current) {
		t.Run("no sync state, "+name, func(t *testing.T) {
			resp := r2Handle(t, r2Handler(&r2Table{t: t}), r2Body(t, cursor))
			r2WantError(t, resp, 400, "a tail page without a sync state ("+cursor+")")
		})
		t.Run("current sync, "+name, func(t *testing.T) {
			resp := r2Handle(t, r2Handler(&r2Table{t: t, syncState: r2SyncState()}), r2Body(t, cursor))
			if resp.StatusCode != 200 {
				t.Errorf("a tail page of the current sync (%s) was answered %d (%s), want 200", cursor, resp.StatusCode, resp.Body)
			}
		})
	}
	for name, cursor := range scaleTailCursors("") {
		want := 409
		if strings.Contains(cursor, `"strategy":3`) {
			want = 200
		}
		t.Run("no seq, "+name, func(t *testing.T) {
			resp := r2Handle(t, r2Handler(&r2Table{t: t, syncState: r2SyncState()}), r2Body(t, cursor))
			if want == 200 {
				if resp.StatusCode != 200 {
					t.Errorf("a strategy 3 tail page without seq (%s) was answered %d (%s), want 200", cursor, resp.StatusCode, resp.Body)
				}
				return
			}
			r2WantError(t, resp, 409, "a tail page without seq of a numbered sync ("+cursor+")")
		})
	}
}

// Tail cursors Rudolph never issues are refused with 400 before anything is read; the shapes it issues are accepted.
func TestScale_ForgedTailCursorsAreABadRequestBeforeAnyRead(t *testing.T) {
	const seq = `,"seq":1790000000000`
	long := `"tail_sk":"` + strings.Repeat("a", maxSortKeyBytes+1) + `"`
	strategies := map[string]string{
		"clean":   `"strategy":1,"batch_size":50,"page":3`,
		"feed":    `"strategy":2,"batch_size":50,"page":3`,
		"machine": `"strategy":3`,
	}
	forged := map[string]string{
		"tail 0 with a tail_sk":  `"tail":0,"tail_sk":"Binary#aa"`,
		"tail_sk without a tail": `"tail_sk":"Binary#aa"`,
		"tail 4":                 `"tail":4`,
		"tail -1":                `"tail":-1`,
		"tail_sk of 1,025 bytes": `"tail":1,` + long,
	}
	keyForged := map[string]map[string]string{
		"clean": {
			"tail with only pk":            `"tail":1,"pk":"GlobalRules"`,
			"tail with only sk":            `"tail":1,"sk":"Binary#aa"`,
			"tail with a feed pk":          `"tail":1,"pk":"RulesFeed","sk":"2000-01-01T00:00:00Z#Binary#aa"`,
			"tail with a machine rules pk": `"tail":1,"pk":"MachineRules#` + inputHandlingMachineID + `","sk":"Binary#aa"`,
		},
		"feed": {
			"tail with only pk":     `"tail":2,"pk":"RulesFeed"`,
			"tail with only sk":     `"tail":2,"sk":"2000-01-01T00:00:00Z#Binary#aa"`,
			"tail with a global pk": `"tail":2,"pk":"GlobalRules","sk":"Binary#aa"`,
		},
	}
	for strategy, fields := range strategies {
		cases := map[string]string{}
		for name, tail := range forged {
			cases[name] = tail
		}
		for name, tail := range keyForged[strategy] {
			cases[name] = tail
		}
		for name, tail := range cases {
			t.Run(strategy+", "+name, func(t *testing.T) {
				cursor := "{" + fields + "," + tail + seq + "}"
				table := &r2Table{t: t, syncState: r2SyncState()}
				resp := r2Handle(t, r2Handler(table), r2Body(t, cursor))
				if resp.StatusCode != 400 || table.queries != 0 || table.gets != 0 {
					t.Errorf("cursor %.200s was answered %d after %d queries and %d reads, want 400 without reading the table",
						cursor, resp.StatusCode, table.queries, table.gets)
				}
			})
		}
	}

	accepted := map[string]string{
		"clean, tail without a key":                 `{"strategy":1,"batch_size":50,"page":3,"tail":1` + seq + `}`,
		"clean, tail 3 with a tail_sk":              `{"strategy":1,"batch_size":50,"page":3,"tail":3,"tail_sk":"Binary#aa"` + seq + `}`,
		"clean, tail with a whole key":              `{"strategy":1,"batch_size":50,"page":3,"pk":"GlobalRules","sk":"Binary#aa","tail":2` + seq + `}`,
		"feed, tail without a key":                  `{"strategy":2,"batch_size":50,"page":3,"tail":2,"tail_sk":"Binary#aa"` + seq + `}`,
		"feed, tail with a whole key":               `{"strategy":2,"batch_size":50,"page":3,"pk":"RulesFeed","sk":"2000-01-01T00:00:00Z#Binary#aa","tail":1` + seq + `}`,
		"feed, tail_sk of exactly 1,024 bytes":      `{"strategy":2,"batch_size":50,"page":3,"tail":1,"tail_sk":"` + strings.Repeat("a", maxSortKeyBytes) + `"` + seq + `}`,
		"machine, tail":                             `{"strategy":3,"tail":1,"tail_sk":"Binary#aa"` + seq + `}`,
		"feed, tail_sk that is not a rule sort key": `{"strategy":2,"batch_size":50,"page":3,"tail":1,"tail_sk":"SyncState"` + seq + `}`,
	}
	for name, cursor := range accepted {
		t.Run("accepted, "+name, func(t *testing.T) {
			resp := r2Handle(t, r2Handler(&r2Table{t: t, syncState: r2SyncState()}), r2Body(t, cursor))
			if resp.StatusCode != 200 {
				t.Errorf("cursor %.200s was answered %d (%s), want 200", cursor, resp.StatusCode, resp.Body)
			}
		})
	}
}

// fill goes through the three sections in order and each in sort key order, whatever page sizes cut them, including
// a cut exactly at the end of a section and an empty section between two others.
func TestScale_TailFillSendsEverySectionOnceInOrder(t *testing.T) {
	entry := func(i int) tailRule {
		id := scaleSHA(i)
		return tailRule{sortKey: rules.RuleSortKeyFromTypeIdentifier(id, rudolphtypes.RuleTypeBinary), rule: rules.SantaRule{RuleType: rudolphtypes.RuleTypeBinary, Policy: rudolphtypes.RulePolicyAllowlist, Identifier: id}}
	}
	one := encodedRuleBytes(RuledownloadRule(entry(1).rule))
	for _, layout := range []struct {
		name     string
		sections [tailReplacements][]int
	}{
		{"all three sections", [tailReplacements][]int{{1, 2, 3}, {4, 5}, {6, 7, 8}}},
		{"empty group section", [tailReplacements][]int{{1, 2}, nil, {3, 4, 5}}},
		{"only replacements", [tailReplacements][]int{nil, nil, {1, 2, 3}}},
	} {
		for perPage := 1; perPage <= 4; perPage++ {
			t.Run(fmt.Sprintf("%s, %d a page", layout.name, perPage), func(t *testing.T) {
				tail := &machineTail{}
				var want []string
				for s, ids := range layout.sections {
					for _, i := range ids {
						tail.sections[s] = append(tail.sections[s], entry(i))
						want = append(want, scaleSHA(i))
					}
				}
				var got []string
				from := tailPosition{}
				for pages := 0; ; pages++ {
					if pages > 20 {
						t.Fatal("the tail never ends")
					}
					page := newRulePage(perPage*one, false)
					next, more := tail.fill(page, from)
					if len(page.rules) == 0 && more {
						t.Fatalf("a page from %+v sent nothing and wants another", from)
					}
					for _, rule := range page.rules {
						got = append(got, rule.Identifier)
					}
					if !more {
						break
					}
					// The position goes into the cursor and comes back through validate
					cursor := ruledownloadCursor{Strategy: ruledownloadStrategyMachine, TailSection: next.section, TailSortKey: next.sortKey}
					if err := cursor.validate(); err != nil {
						t.Fatalf("position %+v does not pass validate: %v", next, err)
					}
					from = next
				}
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("the tail sent %v, want %v", got, want)
				}
			})
		}
	}
}

// A sync of a Santa before 2026.6 (silent_only) gets SILENT_GUI_BLOCKLIST and SILENT_TTY_BLOCKLIST as
// SILENT_BLOCKLIST on every page of the tail, not just the first: the later pages take it from the cursor or the sync
// state, and such a client would skip a rule whose policy it does not know.
func TestScale_SilentOnlyHoldsOnEveryTailPage(t *testing.T) {
	for name, first := range map[string]string{
		"feed sync":                 `{}`,
		"machine page (strategy 3)": `{"cursor":"{\"strategy\":3,\"page\":2,\"batch_size\":50}"}`,
	} {
		t.Run(name, func(t *testing.T) {
			state := r2SyncState()
			state.SilentBlocklistOnly = true
			table := &scaleTable{r4Table: &r4Table{r2Table: &r2Table{t: t, syncState: state}, deliveries: map[string]machinerules.Delivery{}}}
			for i := 1; i <= 6; i++ {
				policy := rudolphtypes.RulePolicySilentGUIBlocklist
				if i%2 == 0 {
					policy = rudolphtypes.RulePolicySilentTTYBlocklist
				}
				table.machineRules = append(table.machineRules, scaleMachineRule(i, policy, ""))
			}
			pages := followSync(t, scaleHandler(table, 300), first)
			if len(pages) < 3 {
				t.Fatalf("the tail took %d pages, want several, so the fixture tests nothing", len(pages))
			}
			sent := 0
			for i, p := range pages {
				for _, rule := range p.rules {
					sent++
					if rule.Policy != rudolphtypes.RulePolicySilentBlocklist {
						t.Errorf("page %d sent %s with policy %v, want SILENT_BLOCKLIST for a silent_only client", i+1, rule.Identifier[:8], rule.Policy)
					}
				}
			}
			if sent != 6 {
				t.Errorf("the sync sent %d rules, want 6", sent)
			}
		})
	}
}
