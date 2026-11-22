package machineconfiguration

import (
	"time"

	"github.com/airbnb/rudolph/pkg/types"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// The fleet job (internal/fleet) works out, for every Mac, the config and client mode preflight would send it now,
// from rows it has already read. These functions are the halves of preflight's own resolution that read nothing, so
// the two cannot drift apart.

// The key of the stored global config row.
const (
	GlobalConfigPartitionKey = globalConfigurationPK
	ConfigSortKey            = currentSK
)

// StoredGlobalConfig decodes the stored global config row as GetGlobalConfig does; nil without one.
func StoredGlobalConfig(item map[string]awstypes.AttributeValue) *MachineConfiguration {
	if len(item) == 0 {
		return nil
	}
	decoded := unmarshalConfigItem(item, globalConfigurationPK)
	return &decoded
}

// IntendedFromStored returns what GetIntendedConfigWithOrigin returns, with the origin of the client mode only, from the
// stored global config (nil without one, see StoredGlobalConfig) and the machine's stored config row (nil without one).
func IntendedFromStored(global *MachineConfiguration, machineItem map[string]awstypes.AttributeValue) (config MachineConfiguration, ownClientMode bool, err error) {
	base := GetUniversalDefaultConfig()
	if global != nil {
		base = *global
	}
	config, err = intendedFrom(base, machineItem)
	return config, OwnClientMode(machineItem), err
}

// ClientModeSource says which setting decides a Mac's client mode.
type ClientModeSource string

const (
	ClientModeFromTemporary ClientModeSource = "temporary"
	ClientModeFromOwn       ClientModeSource = "own"
	ClientModeFromGroup     ClientModeSource = "group"
	ClientModeFromGlobal    ClientModeSource = "global"
	ClientModeFromDefault   ClientModeSource = "default"
)

// IntendedClientMode returns the client mode Rudolph intends for a Mac at now and where it comes from, in the order
// preflight applies: the temporary client mode while it lasts, the machine's own, the strictest of its groups'
// (groupMode, when hasGroupMode), the global config's, the universal default. config and ownClientMode are
// IntendedFromStored's; global is the stored global config, nil without one.
//
// It differs from what preflight sends in one case. A stored mode that is not a client mode (edited by hand) is
// skipped here, so the next setting in the order decides; preflight sends no mode at all for it (SanitizeForSanta), and
// the Mac keeps the mode it has. Naming the mode the remaining settings ask for is what lets a reader see that such a
// Mac is not where its configuration says.
func IntendedClientMode(config MachineConfiguration, ownClientMode bool, global *MachineConfiguration, groupMode types.ClientMode, hasGroupMode bool, now time.Time) (types.ClientMode, ClientModeSource) {
	if mode, ok := config.TemporaryClientModeAt(now); ok {
		return mode, ClientModeFromTemporary
	}
	if ownClientMode {
		return config.ClientMode, ClientModeFromOwn
	}
	if hasGroupMode && validateClientMode(groupMode) == nil {
		return ApplyGroupClientMode(config, ownClientMode, groupMode).ClientMode, ClientModeFromGroup
	}
	if global != nil && validateClientMode(global.ClientMode) == nil {
		return global.ClientMode, ClientModeFromGlobal
	}
	return GetUniversalDefaultConfig().ClientMode, ClientModeFromDefault
}
