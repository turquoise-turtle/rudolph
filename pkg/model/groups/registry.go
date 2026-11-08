package groups

import (
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	registryPK       = "GroupRegistry"
	registrySK       = "Current"
	registryDataType = "GroupRegistry"
)

// Entry is a registered group, as stored in the registry's Groups map under the group's name
type Entry struct {
	Description string `dynamodbav:"Description"`
	// AcceptReported makes the group count for Macs that report it in preflight. Without it only assigned membership
	// counts, for groups whose rules grant privilege or must stick (a local administrator can edit what Santa reports
	// when it comes from a plist).
	AcceptReported bool `dynamodbav:"AcceptReported"`
	// RulesVersion is incremented with every change to the group's rules, in the same transaction, so that servers
	// can cache a group's rules by version
	RulesVersion int    `dynamodbav:"RulesVersion"`
	CreatedAt    string `dynamodbav:"CreatedAt"`
}

// Registry is the one item that lists every registered group
type Registry struct {
	Groups map[string]Entry `dynamodbav:"Groups"`
	// MembershipVersion is incremented with every change to assigned membership or to the registry, for readers
	// (such as rudolph-web) that cache membership
	MembershipVersion int `dynamodbav:"MembershipVersion"`
	// RulesVersionCounter is the last RulesVersion issued to any group. Versions are drawn from it, so a group deleted
	// and created again never repeats a version an earlier group of that name had: servers cache group rules by name
	// and version, and would otherwise serve the deleted group's rules.
	RulesVersionCounter int `dynamodbav:"RulesVersionCounter,omitempty"`
}

// nextRulesVersion returns the version to issue next. Registries without a counter (or groups written by other
// tools) may hold higher versions than the counter, so it starts above all of them.
func (r *Registry) nextRulesVersion() int {
	highest := r.RulesVersionCounter
	for _, entry := range r.Groups {
		highest = max(highest, entry.RulesVersion)
	}
	return highest + 1
}

// counterUnchanged returns the condition that RulesVersionCounter (#c) still has the value read into r, adding the
// value it needs to values
func (r *Registry) counterUnchanged(values map[string]types.AttributeValue) string {
	if r.RulesVersionCounter == 0 {
		return "attribute_not_exists(#c)"
	}
	values[":counter"] = number(r.RulesVersionCounter)
	return "#c = :counter"
}

func registryKey() dynamodb.PrimaryKey {
	return dynamodb.PrimaryKey{PartitionKey: registryPK, SortKey: registrySK}
}

// GetRegistry reads the registry. A missing registry is an empty one.
func GetRegistry(client dynamodb.GetItemAPI, consistent bool) (*Registry, error) {
	out, err := client.GetItem(registryKey(), consistent)
	if err != nil {
		return nil, fmt.Errorf("failed to read the group registry: %w", err)
	}
	registry := &Registry{}
	if len(out.Item) > 0 {
		if err := attributevalue.UnmarshalMap(out.Item, registry); err != nil {
			return nil, fmt.Errorf("failed to unmarshal the group registry: %w", err)
		}
	}
	if registry.Groups == nil {
		registry.Groups = map[string]Entry{}
	}
	return registry, nil
}

// Names returns the registered group names, sorted
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.Groups))
	for name := range r.Groups {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// countable returns how many groups count towards MaxGroups
func (r *Registry) countable() int {
	n := len(r.Groups)
	if _, ok := r.Groups[Ungrouped]; ok {
		n--
	}
	return n
}

// RegistryWriter is what writing the registry, group rules and membership needs
type RegistryWriter interface {
	dynamodb.GetItemAPI
	dynamodb.QueryAPI
	dynamodb.BatchWriteItemAPI
	dynamodb.TransactWriteItemsAPI
	dynamodb.TransactUpdateExpressionAPI
	dynamodb.UpdateExpressionAPI
}

var (
	ErrGroupExists     = errors.New("the group already exists")
	ErrGroupNotFound   = errors.New("no such group")
	ErrTooManyGroups   = fmt.Errorf("at most %d groups can be registered", MaxGroups)
	ErrRegistryChanged = errors.New("the group registry changed while it was being updated; run the command again")
	// ErrDeletedPartly is joined to an error of DeleteGroup after some of the group's rules may have been deleted, so
	// that callers can tell a partial deletion from one that changed nothing
	ErrDeletedPartly = errors.New("some of the group's rules may already be deleted")
)

func names(pairs ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return m
}

func number(n int) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.Itoa(n)}
}

// ensureRegistry creates the registry item, with no groups, when it does not exist
func ensureRegistry(client dynamodb.UpdateExpressionAPI) error {
	_, err := client.UpdateItemExpression(registryKey(),
		"SET #g = if_not_exists(#g, :empty), #d = :dt, #m = if_not_exists(#m, :zero)", "",
		names("#g", "Groups", "#d", "DataType", "#m", "MembershipVersion"),
		map[string]types.AttributeValue{
			":empty": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{}},
			":dt":    &types.AttributeValueMemberS{Value: registryDataType},
			":zero":  number(0),
		})
	if err != nil {
		return fmt.Errorf("failed to create the group registry: %w", err)
	}
	return nil
}

// CreateGroup registers a group. name must already be normalised (see Normalise), or be Ungrouped.
func CreateGroup(timeProvider clock.TimeProvider, client RegistryWriter, name, description string, acceptReported bool) error {
	if err := ensureRegistry(client); err != nil {
		return err
	}
	registry, err := GetRegistry(client, true)
	if err != nil {
		return err
	}
	if _, ok := registry.Groups[name]; ok {
		return ErrGroupExists
	}
	if name != Ungrouped && registry.countable() >= MaxGroups {
		return ErrTooManyGroups
	}
	next := registry.nextRulesVersion()
	entry, err := attributevalue.Marshal(Entry{
		Description:    description,
		AcceptReported: acceptReported,
		RulesVersion:   next,
		CreatedAt:      clock.RFC3339(timeProvider.Now()),
	})
	if err != nil {
		return err
	}
	// The size condition keeps two concurrent creations from exceeding MaxGroups, the counter one from issuing a
	// version twice
	values := map[string]types.AttributeValue{":e": entry, ":one": number(1), ":size": number(len(registry.Groups)), ":next": number(next)}
	_, err = client.UpdateItemExpression(registryKey(),
		"SET #g.#n = :e, #c = :next ADD #m :one",
		"attribute_not_exists(#g.#n) AND size(#g) = :size AND "+registry.counterUnchanged(values),
		names("#g", "Groups", "#n", name, "#m", "MembershipVersion", "#c", "RulesVersionCounter"),
		values)
	if dynamodb.IsConditionalCheckFailed(err) {
		return ErrRegistryChanged
	}
	if err != nil {
		return fmt.Errorf("failed to register the group: %w", err)
	}
	return nil
}

// UpdateGroup changes a group's description (when description is not nil) and whether it accepts reported membership
// (when acceptReported is not nil)
func UpdateGroup(client RegistryWriter, name string, description *string, acceptReported *bool) error {
	update := "ADD #m :one"
	values := map[string]types.AttributeValue{":one": number(1)}
	attrs := names("#g", "Groups", "#n", name, "#m", "MembershipVersion")
	set := ""
	if description != nil {
		set += ", #g.#n.#desc = :desc"
		attrs["#desc"] = "Description"
		values[":desc"] = &types.AttributeValueMemberS{Value: *description}
	}
	if acceptReported != nil {
		set += ", #g.#n.#acc = :acc"
		attrs["#acc"] = "AcceptReported"
		values[":acc"] = &types.AttributeValueMemberBOOL{Value: *acceptReported}
	}
	if set == "" {
		return errors.New("nothing to change: pass --description, --no-reported or --accept-reported")
	}
	update = "SET " + set[2:] + " " + update
	_, err := client.UpdateItemExpression(registryKey(), update, "attribute_exists(#g.#n)", attrs, values)
	if dynamodb.IsConditionalCheckFailed(err) {
		return ErrGroupNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to update the group: %w", err)
	}
	return nil
}

// DeleteGroup removes a group from the registry. A group that still has rules is only deleted with force, which
// deletes its rules first. Assigned membership is left on the Macs' items, where it has no effect while no group of
// that name is registered.
func DeleteGroup(client RegistryWriter, name string, force bool) error {
	keys, err := groupRuleKeys(client, name)
	if err != nil {
		return err
	}
	if len(keys) > 0 {
		if !force {
			return fmt.Errorf("the group has %d rules; remove them first, or pass --force to delete them with the group", len(keys))
		}
		// Rules first: a registry entry left behind by a failure here still shows the group, and running the command
		// again finishes the job
		if err := client.BatchWriteItems(nil, keys); err != nil {
			// They are deleted 25 at a time: a failure after the first batch leaves some deleted
			if len(keys) > 25 {
				return fmt.Errorf("failed to delete the group's rules: %w (%w)", err, ErrDeletedPartly)
			}
			return fmt.Errorf("failed to delete the group's rules: %w", err)
		}
	}
	rulesDeleted := len(keys) > 0
	_, err = client.UpdateItemExpression(registryKey(), "REMOVE #g.#n ADD #m :one", "attribute_exists(#g.#n)",
		names("#g", "Groups", "#n", name, "#m", "MembershipVersion"),
		map[string]types.AttributeValue{":one": number(1)})
	if dynamodb.IsConditionalCheckFailed(err) {
		if rulesDeleted {
			return fmt.Errorf("%w (%w)", ErrGroupNotFound, ErrDeletedPartly)
		}
		return ErrGroupNotFound
	}
	if err != nil {
		if rulesDeleted {
			return fmt.Errorf("failed to delete the group: %w (%w)", err, ErrDeletedPartly)
		}
		return fmt.Errorf("failed to delete the group: %w", err)
	}
	return nil
}

// MembershipChanged records that a Mac's membership changed outside this package, such as when `machine forget` deletes
// its Groups row, so that readers that cache membership see it. It does nothing when there is no group registry.
func MembershipChanged(client dynamodb.UpdateExpressionAPI) error {
	return bumpMembershipVersion(client)
}

// bumpMembershipVersion records that assigned membership changed
func bumpMembershipVersion(client dynamodb.UpdateExpressionAPI) error {
	_, err := client.UpdateItemExpression(registryKey(), "ADD #m :one", "attribute_exists(PK)",
		names("#m", "MembershipVersion"), map[string]types.AttributeValue{":one": number(1)})
	if err != nil && !dynamodb.IsConditionalCheckFailed(err) {
		return fmt.Errorf("failed to record the membership change: %w", err)
	}
	return nil
}
