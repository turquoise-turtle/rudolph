package machineconfiguration

import (
	"errors"
	"testing"
	"time"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// fakeStore is a minimal in-memory GetItem/PutItem/UpdateItem store keyed by PK|SK.
type fakeStore struct {
	items  map[string]map[string]awstypes.AttributeValue
	putErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{items: map[string]map[string]awstypes.AttributeValue{}}
}

func (s *fakeStore) GetItem(key dynamodb.PrimaryKey, _ bool) (*awsdynamodb.GetItemOutput, error) {
	return &awsdynamodb.GetItemOutput{Item: s.items[key.PartitionKey+"|"+key.SortKey]}, nil
}

func (s *fakeStore) PutItem(item interface{}) (*awsdynamodb.PutItemOutput, error) {
	if s.putErr != nil {
		return nil, s.putErr
	}
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return nil, err
	}
	pk := av["PK"].(*awstypes.AttributeValueMemberS).Value
	sk := av["SK"].(*awstypes.AttributeValueMemberS).Value
	s.items[pk+"|"+sk] = av
	return &awsdynamodb.PutItemOutput{}, nil
}

// PutItemIfNotExists emulates DynamoDB's conditional put: when an item with the same primary key already
// exists it fails with the error real DynamoDB returns.
func (s *fakeStore) PutItemIfNotExists(item interface{}) (*awsdynamodb.PutItemOutput, error) {
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return nil, err
	}
	pk := av["PK"].(*awstypes.AttributeValueMemberS).Value
	sk := av["SK"].(*awstypes.AttributeValueMemberS).Value
	if _, exists := s.items[pk+"|"+sk]; exists {
		return nil, &awstypes.ConditionalCheckFailedException{Message: aws.String("The conditional request failed")}
	}
	return s.PutItem(item)
}

func (s *fakeStore) UpdateItem(key dynamodb.PrimaryKey, item interface{}) (*awsdynamodb.UpdateItemOutput, error) {
	fields, err := attributevalue.MarshalMap(item)
	if err != nil {
		return nil, err
	}
	k := key.PartitionKey + "|" + key.SortKey
	current := s.items[k]
	if current == nil {
		current = map[string]awstypes.AttributeValue{}
	}
	for name, v := range fields {
		current[name] = v
	}
	s.items[k] = current
	return &awsdynamodb.UpdateItemOutput{Attributes: current}, nil
}

func (s *fakeStore) config(t *testing.T, pk string) MachineConfiguration {
	t.Helper()
	var c MachineConfiguration
	if err := attributevalue.UnmarshalMap(s.items[pk+"|Config"], &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func fetcherFor(store *fakeStore, tp clock.TimeProvider) ConcreteConfigurationFetcher {
	return ConcreteConfigurationFetcher{
		global:    UncachedConcreteGlobalConfigurationFetcher{getter: store, cache: GetCache(tp)},
		machine:   ConcreteMachineConfigurationFetcher{getter: store},
		universal: ConcreteUniversalConfigurationProvider{},
	}
}

// Shortcoming L1: updating the global blocked path regex must not overwrite the allowed path regex.
func TestGlobalConfigUpdate_BlockedPathRegex_L1(t *testing.T) {
	store := newFakeStore()
	tp := clock.FrozenTimeProvider{Current: clock.Y2KTime()}
	initial := GetUniversalDefaultConfig()
	initial.AllowedPathRegex = "^/Applications/Allowed\\.app/"
	if _, err := store.PutItem(buildConfig(globalConfigurationPK, initial.ClientMode, "", initial.AllowedPathRegex, 50, false, false, false, 600, "")); err != nil {
		t.Fatal(err)
	}

	updater := ConcreteGlobalConfigurationUpdater{updater: store, cache: GetCache(tp), fetcher: fetcherFor(store, tp)}
	blocked := "^/Users/[^/]+/Downloads/"
	if _, err := updater.updateConfig(MachineConfigurationUpdateRequest{BlockedPathRegex: &blocked}); err != nil {
		t.Fatal(err)
	}

	got := store.config(t, globalConfigurationPK)
	if got.BlockedPathRegex != blocked {
		t.Errorf("BlockedPathRegex = %q, want %q", got.BlockedPathRegex, blocked)
	}
	if got.AllowedPathRegex != initial.AllowedPathRegex {
		t.Errorf("AllowedPathRegex was changed to %q, want it to stay %q", got.AllowedPathRegex, initial.AllowedPathRegex)
	}
}

// Shortcoming L4: a failed write must be reported to the caller (the CLI would otherwise print success).
func TestConfigSetters_ReportWriteErrors_L4(t *testing.T) {
	store := newFakeStore()
	store.putErr = errors.New("AccessDeniedException")
	tp := clock.FrozenTimeProvider{Current: clock.Y2KTime()}

	if err := (ConcreteGlobalConfigurationSetter{setter: store, cache: GetCache(tp)}).setGlobalConfig(GetUniversalDefaultConfig()); err == nil {
		t.Error("setGlobalConfig swallowed a DynamoDB error")
	}
	if err := (ConcreteMachineConfigurationSetter{setter: store}).setMachineConfig("AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA", GetUniversalDefaultConfig()); err == nil {
		t.Error("setMachineConfig swallowed a DynamoDB error")
	}
}

// Shortcoming L11: global config changes made elsewhere (CLI, web view) must reach warm Lambdas within a minute.
func TestCachedGlobalConfig_PicksUpChangesWithinAMinute_L11(t *testing.T) {
	store := newFakeStore()
	tm := &clock.TimeMachine{}
	tm.Travel(clock.Y2KTime())
	fetcher := CachedConcreteGlobalConfigurationFetcher{getter: store, cache: GetCache(tm)}

	monitor := GetUniversalDefaultConfig()
	if _, err := store.PutItem(buildConfig(globalConfigurationPK, monitor.ClientMode, "", "", 50, false, false, false, 600, "")); err != nil {
		t.Fatal(err)
	}
	if c, err := fetcher.GetGlobalConfig(); err != nil || c.BatchSize != 50 {
		t.Fatalf("initial fetch = %+v, %v", c, err)
	}

	// Another process changes the config.
	if _, err := store.PutItem(buildConfig(globalConfigurationPK, monitor.ClientMode, "", "", 75, false, false, false, 600, "")); err != nil {
		t.Fatal(err)
	}
	tm.Travel(clock.Y2KTime().Add(61 * time.Second))

	c, err := fetcher.GetGlobalConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.BatchSize != 75 {
		t.Fatalf("config change not visible after 61s (still batch size %d)", c.BatchSize)
	}
}
