package machineconfiguration

import (
	"errors"
	"testing"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/types"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type fakeClient struct {
	dynamodb.DynamoDBClient
	s *fakeStore
}

func (c fakeClient) GetItem(k dynamodb.PrimaryKey, cr bool) (*awsdynamodb.GetItemOutput, error) {
	return c.s.GetItem(k, cr)
}
func (c fakeClient) PutItem(i interface{}) (*awsdynamodb.PutItemOutput, error) { return c.s.PutItem(i) }
func (c fakeClient) UpdateItem(k dynamodb.PrimaryKey, i interface{}) (*awsdynamodb.UpdateItemOutput, error) {
	return c.s.UpdateItem(k, i)
}

// Shortcoming L12: fleet-wide LOCKDOWN is allowed, but only with an explicit confirmation.
func TestGlobalLockdown_RequiresConfirmation_L12(t *testing.T) {
	store := newFakeStore()
	svc := GetMachineConfigurationService(fakeClient{s: store}, clock.FrozenTimeProvider{Current: clock.Y2KTime()})
	lockdown := GetUniversalDefaultConfig()
	lockdown.ClientMode = types.Lockdown

	if err := svc.SetGlobalConfig(lockdown); !errors.Is(err, ErrGlobalLockdownNotConfirmed) {
		t.Fatalf("unconfirmed global lockdown: err = %v, want ErrGlobalLockdownNotConfirmed", err)
	}
	if err := svc.SetGlobalConfig(lockdown, ConfirmGlobalLockdown()); err != nil {
		t.Fatalf("confirmed global lockdown failed: %v", err)
	}
	if got := store.config(t, globalConfigurationPK).ClientMode; got != types.Lockdown {
		t.Fatalf("stored client mode = %v, want LOCKDOWN", got)
	}

	monitor := types.Monitor
	if _, err := svc.UpdateGlobalConfig(MachineConfigurationUpdateRequest{ClientMode: &monitor}); err != nil {
		t.Fatalf("switching back to MONITOR must not need confirmation: %v", err)
	}
	mode := types.Lockdown
	if _, err := svc.UpdateGlobalConfig(MachineConfigurationUpdateRequest{ClientMode: &mode}); !errors.Is(err, ErrGlobalLockdownNotConfirmed) {
		t.Fatalf("unconfirmed update to global lockdown: err = %v", err)
	}
	if _, err := svc.UpdateGlobalConfig(MachineConfigurationUpdateRequest{ClientMode: &mode}, ConfirmGlobalLockdown()); err != nil {
		t.Fatalf("confirmed update to global lockdown failed: %v", err)
	}
}
