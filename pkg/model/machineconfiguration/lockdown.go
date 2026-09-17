package machineconfiguration

import (
	"errors"

	"github.com/airbnb/rudolph/pkg/types"
)

// ErrGlobalLockdownNotConfirmed is returned when a change would put every machine without a machine-specific
// configuration into LOCKDOWN mode, but the caller did not pass ConfirmGlobalLockdown().
var ErrGlobalLockdownNotConfirmed = errors.New("setting the global client mode to LOCKDOWN affects every machine without a machine-specific configuration and must be explicitly confirmed")

// GlobalConfigOption modifies a single SetGlobalConfig or UpdateGlobalConfig call
type GlobalConfigOption func(*globalConfigOptions)

type globalConfigOptions struct {
	confirmGlobalLockdown bool
}

// ConfirmGlobalLockdown confirms that the call it is passed to may set the global client mode to LOCKDOWN
func ConfirmGlobalLockdown() GlobalConfigOption {
	return func(o *globalConfigOptions) {
		o.confirmGlobalLockdown = true
	}
}

func getGlobalConfigOptions(opts []GlobalConfigOption) globalConfigOptions {
	var options globalConfigOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	return options
}

// checkGlobalClientMode is a safety check to prevent accidentally putting the whole fleet into lockdown.
// Nothing prevents this from being done manually in the DynamoDB table itself.
func checkGlobalClientMode(clientMode types.ClientMode, opts []GlobalConfigOption) error {
	if clientMode == types.Lockdown && !getGlobalConfigOptions(opts).confirmGlobalLockdown {
		return ErrGlobalLockdownNotConfirmed
	}
	return nil
}
