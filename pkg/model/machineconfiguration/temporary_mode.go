package machineconfiguration

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// A temporary client mode is stored on a machine's config row as TemporaryClientMode (a number, like ClientMode) and
// TemporaryClientModeExpiresAt (Unix seconds). Until then, preflight sends it instead of the mode the machine would
// otherwise get (its own ClientMode or the inherited global one); afterwards it is ignored, so the Mac returns to that
// mode at its next sync without anything having to clear it. It never changes the machine's own ClientMode.
const (
	temporaryClientModeAttribute          = "TemporaryClientMode"
	temporaryClientModeExpiresAtAttribute = "TemporaryClientModeExpiresAt"
	// MaxTemporaryClientModeDuration bounds how far ahead a temporary mode may end, so a forgotten exception (e.g. a
	// Mac left in monitor mode on a lockdown fleet) does not last indefinitely
	MaxTemporaryClientModeDuration = 30 * 24 * time.Hour
	// MinTemporaryClientModeDuration refuses durations that end before the Mac could even sync (e.g. --for 500ms,
	// meant as 500m)
	MinTemporaryClientModeDuration = time.Minute
	// temporaryClientModeClockSlack allows for a writer whose clock runs ahead of the Lambda's when the 30-day limit is
	// checked again on reading
	temporaryClientModeClockSlack = 10 * time.Minute
)

// ErrNoTemporaryClientMode is returned when a temporary client mode is to be ended on a machine that has none stored
var ErrNoTemporaryClientMode = errors.New("the machine has no temporary client mode")

// TemporaryClientModeStore reads and writes the temporary client mode of a machine's config row
type TemporaryClientModeStore interface {
	dynamodb.GetItemAPI
	dynamodb.PutItemIfNotExistsAPI
	dynamodb.UpdateItemAPI
	dynamodb.UpdateItemRemoveAPI
}

// updateTemporaryClientMode is the fragment written to set a temporary client mode
type updateTemporaryClientMode struct {
	TemporaryClientMode          types.ClientMode `dynamodbav:"TemporaryClientMode"`
	TemporaryClientModeExpiresAt int64            `dynamodbav:"TemporaryClientModeExpiresAt"`
}

// TemporaryClientModeAt returns the temporary client mode stored on the config and whether it applies at now. One
// that has expired, has no expiry, or is not a known mode does not apply. Nor does one ending more than 30 days
// (plus some clock slack) after now: written by other tooling, or in milliseconds (JavaScript's Date.now()), it would
// otherwise make a "temporary" mode practically permanent.
func (c MachineConfiguration) TemporaryClientModeAt(now time.Time) (types.ClientMode, bool) {
	if c.TemporaryClientMode == 0 || c.TemporaryClientModeExpiresAt <= 0 {
		return 0, false
	}
	if validateClientMode(c.TemporaryClientMode) != nil {
		return 0, false
	}
	if !now.Before(c.TemporaryClientModeEnd()) || temporaryClientModeTooFarAhead(c.TemporaryClientModeExpiresAt, now) {
		return 0, false
	}
	return c.TemporaryClientMode, true
}

func temporaryClientModeTooFarAhead(expiresAt int64, now time.Time) bool {
	return expiresAt > now.Add(MaxTemporaryClientModeDuration+temporaryClientModeClockSlack).Unix()
}

// TemporaryClientModeEnd returns the end of the stored temporary client mode as a time
func (c MachineConfiguration) TemporaryClientModeEnd() time.Time {
	return time.Unix(c.TemporaryClientModeExpiresAt, 0)
}

// ApplyTemporaryClientMode returns the config with the client mode Santa is to use at now: the temporary client mode
// while it applies, else the config's own. The temporary attributes are kept, for display.
func ApplyTemporaryClientMode(config MachineConfiguration, now time.Time) MachineConfiguration {
	mode, ok := config.TemporaryClientModeAt(now)
	if ok {
		config.ClientMode = mode
	} else if config.TemporaryClientMode != 0 {
		if err := validateClientMode(config.TemporaryClientMode); err != nil {
			log.Printf("ignoring stored temporary client mode: %v", err)
		} else if config.TemporaryClientModeExpiresAt <= 0 {
			log.Printf("ignoring stored temporary client mode %d without an expiry", config.TemporaryClientMode)
		} else if temporaryClientModeTooFarAhead(config.TemporaryClientModeExpiresAt, now) {
			log.Printf("ignoring stored temporary client mode %d ending at %d, more than 30 days ahead (an expiry in milliseconds?)", config.TemporaryClientMode, config.TemporaryClientModeExpiresAt)
		}
	}
	return config
}

// ValidateTemporaryClientMode checks a temporary client mode that is to end at expires, requested at now: a known
// mode, ending between MinTemporaryClientModeDuration and MaxTemporaryClientModeDuration ahead.
func ValidateTemporaryClientMode(mode types.ClientMode, expires time.Time, now time.Time) error {
	if err := validateClientMode(mode); err != nil {
		return err
	}
	if !expires.After(now) {
		return fmt.Errorf("a temporary client mode must end in the future, not at %s", expires.Format(time.RFC3339))
	}
	if expires.Sub(now) < MinTemporaryClientModeDuration {
		return errors.New("a temporary client mode must last at least a minute")
	}
	if expires.Sub(now) > MaxTemporaryClientModeDuration {
		return fmt.Errorf("a temporary client mode can last at most %d days", int(MaxTemporaryClientModeDuration/(24*time.Hour)))
	}
	return nil
}

// SetTemporaryClientMode stores a temporary client mode on the machine's config row, replacing any earlier one. A
// machine without a config row gets one holding only the temporary mode, so it keeps inheriting every other setting.
// The machine's own ClientMode is left as it is.
func SetTemporaryClientMode(client TemporaryClientModeStore, machineID string, mode types.ClientMode, expires time.Time, now time.Time) error {
	if err := ValidateTemporaryClientMode(mode, expires, now); err != nil {
		return err
	}
	fragment := updateTemporaryClientMode{TemporaryClientMode: mode, TemporaryClientModeExpiresAt: expires.Unix()}
	key := dynamodb.PrimaryKey{PartitionKey: machineConfigurationPK(machineID), SortKey: machineConfigurationSK()}
	for attempt := 1; ; attempt++ {
		output, err := client.GetItem(key, true)
		if err != nil {
			return fmt.Errorf("failed to read the machine configuration: %w", err)
		}
		if len(output.Item) > 0 {
			_, err = client.UpdateItem(key, fragment)
		} else {
			_, err = client.PutItemIfNotExists(map[string]interface{}{
				"PK":                                  key.PartitionKey,
				"SK":                                  key.SortKey,
				"DataType":                            types.DataTypeMachineConfig,
				temporaryClientModeAttribute:          fragment.TemporaryClientMode,
				temporaryClientModeExpiresAtAttribute: fragment.TemporaryClientModeExpiresAt,
			})
		}
		// The row was created or deleted in the meantime; write to its current state
		if dynamodb.IsConditionalCheckFailed(err) && attempt < maxConfigWriteAttempts {
			continue
		}
		if err != nil {
			return fmt.Errorf("failed to store the temporary client mode: %w", err)
		}
		return nil
	}
}

// EndTemporaryClientMode removes the temporary client mode from the machine's config row, so the machine gets its
// usual mode again at its next sync. The rest of the row is left as it is, even when the row holds nothing else: an
// empty machine config inherits every setting, like no config. ErrNoTemporaryClientMode is returned when the row holds
// no temporary client mode.
func EndTemporaryClientMode(client TemporaryClientModeStore, machineID string) error {
	key := dynamodb.PrimaryKey{PartitionKey: machineConfigurationPK(machineID), SortKey: machineConfigurationSK()}
	condition := expression.AttributeExists(expression.Name(temporaryClientModeAttribute)).
		Or(expression.AttributeExists(expression.Name(temporaryClientModeExpiresAtAttribute)))
	_, err := client.UpdateItemRemoveIf(key, map[string]interface{}{}, []string{temporaryClientModeAttribute, temporaryClientModeExpiresAtAttribute}, condition)
	if dynamodb.IsConditionalCheckFailed(err) {
		return ErrNoTemporaryClientMode
	}
	if err != nil {
		return fmt.Errorf("failed to end the temporary client mode: %w", err)
	}
	return nil
}

// withoutTemporaryClientMode returns the config without a temporary client mode. A temporary mode is a setting of a
// machine only; the global config never has one (other than written by hand), and would otherwise pass it on.
func withoutTemporaryClientMode(config MachineConfiguration) MachineConfiguration {
	config.TemporaryClientMode = 0
	config.TemporaryClientModeExpiresAt = 0
	return config
}

// TemporaryClientModeSnapshot is the temporary client mode of a machine's config row as it was read, attribute by
// attribute and undecoded, so a later write can require the row to still hold exactly that
type TemporaryClientModeSnapshot struct {
	mode    awstypes.AttributeValue
	expires awstypes.AttributeValue
}

// ReadTemporaryClientModeSnapshot reads the temporary client mode attributes of the machine's config row
func ReadTemporaryClientModeSnapshot(client dynamodb.GetItemAPI, machineID string) (TemporaryClientModeSnapshot, error) {
	output, err := client.GetItem(dynamodb.PrimaryKey{PartitionKey: machineConfigurationPK(machineID), SortKey: machineConfigurationSK()}, true)
	if err != nil {
		return TemporaryClientModeSnapshot{}, fmt.Errorf("failed to read the machine configuration: %w", err)
	}
	return TemporaryClientModeSnapshot{
		mode:    output.Item[temporaryClientModeAttribute],
		expires: output.Item[temporaryClientModeExpiresAtAttribute],
	}, nil
}

// Empty reports whether the row had no temporary client mode attributes (or no row), so ending one changes nothing
func (s TemporaryClientModeSnapshot) Empty() bool {
	return s.mode == nil && s.expires == nil
}

// condition holds while both attributes are as in the snapshot (absent where they were absent)
func (s TemporaryClientModeSnapshot) condition() expression.ConditionBuilder {
	return unchangedAttribute(temporaryClientModeAttribute, s.mode).And(unchangedAttribute(temporaryClientModeExpiresAtAttribute, s.expires))
}

func unchangedAttribute(name string, value awstypes.AttributeValue) expression.ConditionBuilder {
	if value == nil {
		return expression.AttributeNotExists(expression.Name(name))
	}
	return expression.Equal(expression.Name(name), expression.Value(rawAttributeValue{value}))
}

// rawAttributeValue passes an attribute value read from DynamoDB into an expression unchanged
type rawAttributeValue struct {
	value awstypes.AttributeValue
}

func (r rawAttributeValue) MarshalDynamoDBAttributeValue() (awstypes.AttributeValue, error) {
	return r.value, nil
}

// ErrTemporaryClientModeChanged is returned when a machine config is to be replaced, but its temporary client mode
// changed since it was read
var ErrTemporaryClientModeChanged = errors.New("the temporary client mode of the machine changed (it was set or ended) while the configuration was being replaced; nothing was written")
