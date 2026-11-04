package machineconfiguration

import (
	"errors"
	"fmt"
	"log"
	"math"

	"github.com/airbnb/rudolph/pkg/types"
)

// Limits of the settings sent to Santa (santa.sync.v1.PreflightResponse)
const (
	// DefaultBatchSize is used when no valid batch size is configured; it is also Santa's own default
	DefaultBatchSize = 50
	MinBatchSize     = 1
	// MaxBatchSize bounds the number of events Santa uploads per request (and the rules per rule download page)
	MaxBatchSize = 500
	// MinFullSyncInterval is the lowest full sync interval Santa accepts, in seconds
	MinFullSyncInterval = 60
	// MaxFullSyncInterval is the largest value of the uint32 full_sync_interval_seconds field
	MaxFullSyncInterval = math.MaxUint32
	// MaxRemountUsbFlags bounds the remount_usb_mode list; macOS has far fewer mount flags than this
	MaxRemountUsbFlags = 32
)

// ErrGlobalCleanSync is returned when the clean sync flag is set on the global config: it would make every machine
// clean sync at once. A clean sync is requested per machine.
var ErrGlobalCleanSync = errors.New("clean sync can only be requested for a specific machine (--machine); a global clean sync flag would make the whole fleet clean sync at once")

func validateClientMode(mode types.ClientMode) error {
	switch mode {
	case types.Monitor, types.Lockdown, types.Standalone:
		return nil
	default:
		return fmt.Errorf("invalid client mode %d; must be MONITOR (1), LOCKDOWN (2) or STANDALONE (3)", mode)
	}
}

func validateBatchSize(batchSize int) error {
	if batchSize < MinBatchSize || batchSize > MaxBatchSize {
		return fmt.Errorf("invalid batch size %d; must be between %d and %d", batchSize, MinBatchSize, MaxBatchSize)
	}
	return nil
}

func validateFullSyncInterval(seconds int) error {
	if seconds < MinFullSyncInterval || int64(seconds) > MaxFullSyncInterval {
		return fmt.Errorf("invalid full sync interval %d seconds; must be between %d and %d", seconds, MinFullSyncInterval, int64(MaxFullSyncInterval))
	}
	return nil
}

func validateRemountUsbFlagCount(flags []string) error {
	if len(flags) > MaxRemountUsbFlags {
		return fmt.Errorf("too many remount USB mode flags (%d); at most %d are allowed", len(flags), MaxRemountUsbFlags)
	}
	return nil
}

// ValidateConfig checks the values of a complete configuration before it is written. A batch size or full sync
// interval of 0 means "not configured" (the default is used). global reports whether the config is the global one.
// Remount flags are checked against the USB mount blocking when the config is written, not here.
func ValidateConfig(config MachineConfiguration, global bool) error {
	if err := validateClientMode(config.ClientMode); err != nil {
		return err
	}
	if config.BatchSize != 0 {
		if err := validateBatchSize(config.BatchSize); err != nil {
			return err
		}
	}
	if config.FullSyncInterval != 0 {
		if err := validateFullSyncInterval(config.FullSyncInterval); err != nil {
			return err
		}
	}
	if _, err := normalizeOverrideFileAccessAction(config.OverrideFileAccessAction); err != nil {
		return err
	}
	if err := validateRemountUsbFlagCount(config.RemountUsbMode); err != nil {
		return err
	}
	if global && config.CleanSync {
		return ErrGlobalCleanSync
	}
	return nil
}

// ValidateUpdateRequest checks the values of an update before it is applied. global reports whether the global
// config is updated. Clearing the clean sync flag is allowed everywhere.
func ValidateUpdateRequest(request MachineConfigurationUpdateRequest, global bool) error {
	if request.ClientMode != nil {
		if err := validateClientMode(*request.ClientMode); err != nil {
			return err
		}
	}
	if request.BatchSize != nil {
		if err := validateBatchSize(*request.BatchSize); err != nil {
			return err
		}
	}
	if request.FullSyncInterval != nil {
		if err := validateFullSyncInterval(*request.FullSyncInterval); err != nil {
			return err
		}
	}
	if request.OverrideFileAccessAction != nil {
		if _, err := normalizeOverrideFileAccessAction(*request.OverrideFileAccessAction); err != nil {
			return err
		}
	}
	if request.RemountUsbMode != nil {
		if err := validateRemountUsbFlagCount(*request.RemountUsbMode); err != nil {
			return err
		}
	}
	if global && request.CleanSync != nil && *request.CleanSync {
		return ErrGlobalCleanSync
	}
	return validateInherit(request, global)
}

// SanitizeForSanta returns the config with every value Santa cannot use replaced by a safe default, logging each
// replacement. Stored rows may hold such values (edited by hand, written by other tooling or an older Rudolph);
// sending them would make Santa reject the whole preflight response, on every machine using that config.
//   - an unknown client mode is left out (0), so Santa keeps its current mode
//   - a batch size out of range becomes DefaultBatchSize
//   - a full sync interval out of range is left out (0), so Santa uses its default
//   - an unknown file access action is left out; a legacy spelling is converted to the enum name
//   - a remount flag list that is too long or cannot be decoded is left out, and so is block_usb_mount when it is
//     true (Santa would block USB mass storage instead of remounting it)
func SanitizeForSanta(config MachineConfiguration) MachineConfiguration {
	if config.ClientMode != 0 {
		if err := validateClientMode(config.ClientMode); err != nil {
			log.Printf("ignoring stored client mode: %v", err)
			config.ClientMode = 0
		}
	}
	if err := validateBatchSize(config.BatchSize); err != nil {
		if config.BatchSize != 0 {
			log.Printf("using batch size %d instead of the stored value: %v", DefaultBatchSize, err)
		}
		config.BatchSize = DefaultBatchSize
	}
	if config.FullSyncInterval != 0 {
		if err := validateFullSyncInterval(config.FullSyncInterval); err != nil {
			log.Printf("ignoring stored full sync interval: %v", err)
			config.FullSyncInterval = 0
		}
	}
	action, err := normalizeOverrideFileAccessAction(config.OverrideFileAccessAction)
	if err != nil {
		log.Printf("ignoring stored override file access action: %v", err)
	}
	config.OverrideFileAccessAction = action
	remountUsbModeDropped := config.remountUsbModeDropped
	if err := validateRemountUsbFlagCount(config.RemountUsbMode); err != nil {
		log.Printf("ignoring stored remount USB mode: %v", err)
		remountUsbModeDropped = true
	}
	if remountUsbModeDropped {
		config.RemountUsbMode = nil
		// Santa keeps its current remount flags when none are sent, or has none, and would then block USB mass
		// storage outright where the config meant to remount it with restrictions. Leaving block_usb_mount out as
		// well keeps the Mac's current USB behaviour until the config is fixed.
		if config.BlockUsbMount != nil && *config.BlockUsbMount {
			log.Printf("ignoring stored block USB mount, whose remount flags were ignored")
			config.BlockUsbMount = nil
		}
	}
	config.remountUsbModeDropped = false
	return config
}
