// Package fleethealth is the setting behind the fleet health lists (internal/fleet, docs/fleet-health.md): after how
// many days without a completed sync a Mac counts as stale, holiday mode, which raises that threshold until a date
// and then lapses by itself, and the minimum Santa version, below which a Mac is listed under Version. The CLI
// (rudolph fleet holiday, rudolph fleet min-santa-version) and rudolph-web write it through this package; the fleet
// job reads it on every run. It is one item of the store table:
//
//	PK: FleetHealth, SK: Config
//	StaleDays         the normal threshold in days; DefaultStaleDays when absent
//	HolidayStaleDays  the threshold while the holiday lasts
//	HolidayUntil      the holiday's last day, a UTC date (2006-01-02), inclusive
//	MinSantaVersion   a release such as 2026.10, or "off"; DefaultMinSantaVersion when absent
//
// The two holiday attributes are absent when no holiday is set. Callers record their own audit entry.
package fleethealth

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	rudolphtypes "github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	PartitionKey = "FleetHealth"
	SortKey      = "Config"

	// DefaultStaleDays is the normal threshold when none is stored.
	DefaultStaleDays = 10
	MinStaleDays     = 1
	MaxStaleDays     = 365
	// MaxHolidayDaysAhead bounds how far ahead a holiday may end, so a mistyped year does not silence the stale list
	// for good.
	MaxHolidayDaysAhead = 366

	DayLayout = "2006-01-02"

	// DefaultMinSantaVersion is the minimum Santa version when none is stored. It is the one default: the fleet job,
	// the CLI and rudolph-web all read it from here.
	DefaultMinSantaVersion = "2026.9"
	// MinSantaVersionOff is the stored value that turns the Version list off.
	MinSantaVersionOff = "off"
	// MaxMinSantaVersionBytes bounds a stored release: it is copied to the fleet pointer and shown on every page view.
	MaxMinSantaVersionBytes = 32

	// A holiday is checked against the normal threshold it was read with; the write is tried again when that changed
	// in between.
	maxWriteAttempts = 3
)

var (
	// ErrNoHoliday is returned by EndHoliday when the item holds no holiday, in force or lapsed.
	ErrNoHoliday = errors.New("no holiday is set")
	// ErrChanged is returned when the setting kept changing while a holiday was being stored.
	ErrChanged = errors.New("the fleet health setting changed while the holiday was being stored; try again")
	// ErrNoMinSantaVersion is returned by ClearMinSantaVersion when no minimum Santa version is stored.
	ErrNoMinSantaVersion = errors.New("no minimum Santa version is stored")
)

// Config is the setting as stored. A zero field is an absent attribute, or one holding a value that cannot be used
// (see FromItem).
type Config struct {
	StaleDays        int
	HolidayStaleDays int
	HolidayUntil     string
	// MinSantaVersion is a release ("2026.10") or "off"; "" when absent or unusable. Read it through
	// MinSantaVersionInForce, which applies the default.
	MinSantaVersion string
}

// Reader and Writer are what reading and changing the setting need: Rudolph's DynamoDB client (pkg/dynamodb).
type Reader interface {
	dynamodb.GetItemAPI
}

type Writer interface {
	dynamodb.GetItemAPI
	dynamodb.UpdateExpressionAPI
}

// Key is the key of the item
func Key() dynamodb.PrimaryKey {
	return dynamodb.PrimaryKey{PartitionKey: PartitionKey, SortKey: SortKey}
}

// FromItem decodes the item; a missing item is the zero Config. An attribute that cannot be used (not a number, days
// outside MinStaleDays to MaxStaleDays, a date that is not one, a minimum Santa version ValidateMinSantaVersion
// refuses) is dropped rather than refused, and a holiday needs both of its attributes: the fleet job runs unattended,
// and a hand-edited value must fall back to the default instead of stopping every run.
func FromItem(item map[string]types.AttributeValue) Config {
	config := Config{StaleDays: days(item["StaleDays"])}
	holidayDays := days(item["HolidayStaleDays"])
	until, _ := item["HolidayUntil"].(*types.AttributeValueMemberS)
	if holidayDays != 0 && until != nil {
		if _, err := time.Parse(DayLayout, until.Value); err == nil {
			config.HolidayStaleDays, config.HolidayUntil = holidayDays, until.Value
		}
	}
	if version, ok := item["MinSantaVersion"].(*types.AttributeValueMemberS); ok {
		config.MinSantaVersion, _ = ValidateMinSantaVersion(version.Value)
	}
	return config
}

func days(value types.AttributeValue) int {
	n, ok := value.(*types.AttributeValueMemberN)
	if !ok {
		return 0
	}
	d, err := strconv.Atoi(n.Value)
	if err != nil || d < MinStaleDays || d > MaxStaleDays {
		return 0
	}
	return d
}

// Get reads the setting
func Get(client Reader, consistent bool) (Config, error) {
	_, config, err := get(client, consistent)
	return config, err
}

func get(client Reader, consistent bool) (map[string]types.AttributeValue, Config, error) {
	out, err := client.GetItem(Key(), consistent)
	if err != nil {
		return nil, Config{}, fmt.Errorf("failed to read the fleet health setting: %w", err)
	}
	return out.Item, FromItem(out.Item), nil
}

// NormalStaleDays is the threshold outside a holiday
func (c Config) NormalStaleDays() int {
	if c.StaleDays == 0 {
		return DefaultStaleDays
	}
	return c.StaleDays
}

// HolidayOn reports whether a holiday is in force on the UTC date of now: through HolidayUntil, and not after it.
func (c Config) HolidayOn(now time.Time) bool {
	return c.HolidayStaleDays != 0 && Today(now) <= c.HolidayUntil
}

// InForce returns the stale threshold in days on the UTC date of now, and whether it is the holiday's.
func (c Config) InForce(now time.Time) (staleDays int, holiday bool) {
	if c.HolidayOn(now) {
		return c.HolidayStaleDays, true
	}
	return c.NormalStaleDays(), false
}

// Today is the UTC date of now, in the form HolidayUntil is stored in
func Today(now time.Time) string {
	return now.UTC().Format(DayLayout)
}

// ValidateHoliday checks a holiday of staleDays lasting through until (a UTC date, 2006-01-02), requested at now
// while c is stored: MinStaleDays to MaxStaleDays and at least the normal threshold (a holiday never makes the list
// stricter), ending today or later and at most MaxHolidayDaysAhead days from today.
func (c Config) ValidateHoliday(staleDays int, until string, now time.Time) error {
	if staleDays < MinStaleDays || staleDays > MaxStaleDays {
		return fmt.Errorf("stale days must be between %d and %d, not %d", MinStaleDays, MaxStaleDays, staleDays)
	}
	if normal := c.NormalStaleDays(); staleDays < normal {
		return fmt.Errorf("a holiday's stale days (%d) cannot be below the normal threshold of %d days", staleDays, normal)
	}
	end, err := time.Parse(DayLayout, until)
	if err != nil {
		return fmt.Errorf("the holiday's last day must be a date as YYYY-MM-DD, not %q", until)
	}
	today, _ := time.Parse(DayLayout, Today(now))
	if end.Before(today) {
		return fmt.Errorf("the holiday's last day %s is in the past (today is %s, UTC)", until, Today(now))
	}
	if end.After(today.AddDate(0, 0, MaxHolidayDaysAhead)) {
		return fmt.Errorf("the holiday's last day %s is more than %d days ahead", until, MaxHolidayDaysAhead)
	}
	return nil
}

// SetHoliday stores a holiday, replacing any earlier one, after ValidateHoliday against the stored setting at the
// clock's time. The normal threshold is left as it is. It returns the setting as stored.
func SetHoliday(timeProvider clock.TimeProvider, client Writer, staleDays int, until string) (Config, error) {
	for attempt := 1; ; attempt++ {
		item, config, err := get(client, true)
		if err != nil {
			return Config{}, err
		}
		if err := config.ValidateHoliday(staleDays, until, timeProvider.Now()); err != nil {
			return Config{}, err
		}
		values := map[string]types.AttributeValue{
			":days":  &types.AttributeValueMemberN{Value: strconv.Itoa(staleDays)},
			":until": &types.AttributeValueMemberS{Value: until},
		}
		// The holiday was checked against the normal threshold just read; it must still be that one when stored.
		condition := "attribute_not_exists(StaleDays)"
		if stored, ok := item["StaleDays"]; ok {
			condition = "StaleDays = :normal"
			values[":normal"] = stored
		}
		_, err = client.UpdateItemExpression(Key(), "SET HolidayStaleDays = :days, HolidayUntil = :until", condition, nil, values)
		if dynamodb.IsConditionalCheckFailed(err) {
			if attempt < maxWriteAttempts {
				continue
			}
			return Config{}, ErrChanged
		}
		if err != nil {
			return Config{}, fmt.Errorf("failed to store the holiday: %w", err)
		}
		config.HolidayStaleDays, config.HolidayUntil = staleDays, until
		return config, nil
	}
}

// EndHoliday removes the holiday, in force or lapsed, and leaves the normal threshold as it is. It returns the setting
// as it was, so the caller can say which holiday ended, or ErrNoHoliday when the item holds none (nothing is written
// then).
func EndHoliday(client Writer) (Config, error) {
	_, before, err := get(client, true)
	if err != nil {
		return Config{}, err
	}
	// Conditional on the raw attributes, not on what decoded: a holiday stored with a value that cannot be used is
	// removed too.
	_, err = client.UpdateItemExpression(Key(), "REMOVE HolidayStaleDays, HolidayUntil",
		"attribute_exists(HolidayStaleDays) OR attribute_exists(HolidayUntil)", nil, nil)
	if dynamodb.IsConditionalCheckFailed(err) {
		return Config{}, ErrNoHoliday
	}
	if err != nil {
		return Config{}, fmt.Errorf("failed to end the holiday: %w", err)
	}
	return before, nil
}

// ValidateMinSantaVersion is the one rule for what may be stored as the minimum Santa version, and returns it as it is
// stored: surrounding spaces trimmed, "off" in any case as MinSantaVersionOff, and a release (anything
// rudolphtypes.ParseSantaVersion reads as one, in printable ASCII, at most MaxMinSantaVersionBytes) as typed.
func ValidateMinSantaVersion(version string) (string, error) {
	trimmed := strings.TrimSpace(version)
	if strings.EqualFold(trimmed, MinSantaVersionOff) {
		return MinSantaVersionOff, nil
	}
	refused := fmt.Errorf("the minimum Santa version must be a release such as 2026.10, or %s, not %q", MinSantaVersionOff, version)
	if len(trimmed) > MaxMinSantaVersionBytes {
		return "", fmt.Errorf("%w (at most %d characters)", refused, MaxMinSantaVersionBytes)
	}
	// ParseSantaVersion stops reading at a space or a suffix, so what follows the release could be anything. The
	// value is shown as stored by the CLI and the web view: nothing a terminal acts on, or that hides text, gets in.
	for _, r := range trimmed {
		if r < 0x20 || r > 0x7e {
			return "", refused
		}
	}
	if rudolphtypes.ParseSantaVersion(trimmed).IsCurrent() {
		return "", refused
	}
	return trimmed, nil
}

// MinSantaVersionInForce is the minimum Santa version the fleet job uses and every display shows: the stored value,
// or DefaultMinSantaVersion when none is stored or the stored one cannot be used. off reports that the Version list
// is turned off (version is then MinSantaVersionOff); stored reports that version came from the item, not the
// default.
func (c Config) MinSantaVersionInForce() (version string, off bool, stored bool) {
	if c.MinSantaVersion == "" {
		return DefaultMinSantaVersion, false, false
	}
	return c.MinSantaVersion, c.MinSantaVersion == MinSantaVersionOff, true
}

// SetMinSantaVersion stores version, as ValidateMinSantaVersion returns it, replacing any earlier value and creating
// the item when there is none. The stale threshold and the holiday are left as they are. It returns the setting as
// stored; a refused version writes nothing.
func SetMinSantaVersion(client Writer, version string) (Config, error) {
	version, err := ValidateMinSantaVersion(version)
	if err != nil {
		return Config{}, err
	}
	_, config, err := get(client, true)
	if err != nil {
		return Config{}, err
	}
	_, err = client.UpdateItemExpression(Key(), "SET MinSantaVersion = :version", "", nil,
		map[string]types.AttributeValue{":version": &types.AttributeValueMemberS{Value: version}})
	if err != nil {
		return Config{}, fmt.Errorf("failed to store the minimum Santa version: %w", err)
	}
	config.MinSantaVersion = version
	return config, nil
}

// ClearMinSantaVersion removes the stored minimum Santa version, so DefaultMinSantaVersion applies again, and leaves
// the stale threshold and the holiday as they are. It returns the setting as it was, so the caller can say what was
// cleared, or ErrNoMinSantaVersion when the item holds none (nothing is written then).
func ClearMinSantaVersion(client Writer) (Config, error) {
	_, before, err := get(client, true)
	if err != nil {
		return Config{}, err
	}
	// Conditional on the raw attribute, not on what decoded: a value that cannot be used is removed too. The
	// condition also keeps the update from creating an empty item.
	_, err = client.UpdateItemExpression(Key(), "REMOVE MinSantaVersion", "attribute_exists(MinSantaVersion)", nil, nil)
	if dynamodb.IsConditionalCheckFailed(err) {
		return Config{}, ErrNoMinSantaVersion
	}
	if err != nil {
		return Config{}, fmt.Errorf("failed to clear the minimum Santa version: %w", err)
	}
	return before, nil
}
