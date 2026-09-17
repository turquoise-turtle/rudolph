package config

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/spf13/cobra"
)

const (
	flagBlockedPaths              = "blocked-paths"
	flagAllowedPaths              = "allowed-paths"
	flagConfirmGlobalLockdown     = "confirm-global-lockdown"
	flagEnableAllEventUpload      = "enable-all-event-upload"
	flagDisableUnknownEventUpload = "disable-unknown-event-upload"
	flagOverrideFileAccessAction  = "override-file-access-action"
	flagBlockUsbMount             = "block-usb-mount"
	flagRemountUsbMode            = "remount-usb-mode"
	flagEventDetailURL            = "event-detail-url"
	flagEventDetailText           = "event-detail-text"
)

// combinePathRegexes combines several path regexes into one regex that matches a path if and only if any of
// them matches. Each pattern is validated on its own. Empty patterns are ignored, and no patterns result in ""
// which clears the setting on Santa.
func combinePathRegexes(patterns []string) (string, error) {
	var groups []string
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return "", fmt.Errorf("invalid path regex %q: %w", pattern, err)
		}
		groups = append(groups, pattern)
	}

	if len(groups) == 1 {
		return groups[0], nil
	}
	for i, pattern := range groups {
		// Non-capturing groups keep each pattern's alternations and anchors to itself
		groups[i] = "(?:" + pattern + ")"
	}
	combined := strings.Join(groups, "|")
	if _, err := regexp.Compile(combined); err != nil {
		return "", fmt.Errorf("path regexes cannot be combined: %w", err)
	}
	return combined, nil
}

// pathRegexFlags are the repeatable --blocked-paths and --allowed-paths flags
type pathRegexFlags struct {
	blocked []string
	allowed []string
}

func (f *pathRegexFlags) addFlags(cmd *cobra.Command) {
	cmd.Flags().StringArrayVarP(&f.blocked, flagBlockedPaths, "b", nil, `A regex of paths to be blocked. Repeat the flag for several regexes (each value is one regex, commas included); a path is blocked if any of them matches`)
	cmd.Flags().StringArrayVarP(&f.allowed, flagAllowedPaths, "a", nil, `A regex of paths to be allowed. Repeat the flag for several regexes (each value is one regex, commas included); a path is allowed if any of them matches`)
}

func (f *pathRegexFlags) blockedPathRegex() (string, error) {
	regex, err := combinePathRegexes(f.blocked)
	if err != nil {
		return "", fmt.Errorf("--%s: %w", flagBlockedPaths, err)
	}
	return regex, nil
}

func (f *pathRegexFlags) allowedPathRegex() (string, error) {
	regex, err := combinePathRegexes(f.allowed)
	if err != nil {
		return "", fmt.Errorf("--%s: %w", flagAllowedPaths, err)
	}
	return regex, nil
}

// santaSettingsFlags are flags for optional Santa settings. A setting is only configured when its flag is given;
// otherwise Santa keeps its current value.
type santaSettingsFlags struct {
	enableAllEventUpload      bool
	disableUnknownEventUpload bool
	overrideFileAccessAction  string
	blockUsbMount             bool
	remountUsbMode            []string
	eventDetailURL            string
	eventDetailText           string
}

func (f *santaSettingsFlags) addFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.enableAllEventUpload, flagEnableAllEventUpload, false, "Upload events for all executions, not only for blocked (or would-be blocked) ones")
	cmd.Flags().BoolVar(&f.disableUnknownEventUpload, flagDisableUnknownEventUpload, false, "Do not upload events for unknown binaries that would be blocked in lockdown mode")
	cmd.Flags().StringVar(&f.overrideFileAccessAction, flagOverrideFileAccessAction, "", `Override the File Access Authorization action. valid options are: "none", "audit_only" or "disable"`)
	cmd.Flags().BoolVar(&f.blockUsbMount, flagBlockUsbMount, false, "Block mounting of USB mass storage devices")
	cmd.Flags().StringSliceVar(&f.remountUsbMode, flagRemountUsbMode, nil, `Mount flags (e.g. "rdonly,noexec") to remount blocked USB mass storage devices with. Requires --block-usb-mount`)
	cmd.Flags().StringVar(&f.eventDetailURL, flagEventDetailURL, "", "URL opened by the button of a block notification. Supports Santa's placeholders such as %file_sha%")
	cmd.Flags().StringVar(&f.eventDetailText, flagEventDetailText, "", "Text of the button of a block notification")
}

// updateRequest adds the settings whose flags were given to the request
func (f *santaSettingsFlags) updateRequest(cmd *cobra.Command, request *machineconfiguration.MachineConfigurationUpdateRequest) error {
	flags := cmd.Flags()
	if flags.Changed(flagEnableAllEventUpload) {
		request.EnableAllEventUpload = &f.enableAllEventUpload
	}
	if flags.Changed(flagDisableUnknownEventUpload) {
		request.DisableUnknownEventUpload = &f.disableUnknownEventUpload
	}
	if flags.Changed(flagOverrideFileAccessAction) {
		action, err := types.ParseFileAccessAction(f.overrideFileAccessAction)
		if err != nil {
			return fmt.Errorf("--%s: %w", flagOverrideFileAccessAction, err)
		}
		request.OverrideFileAccessAction = &action
	}
	if flags.Changed(flagBlockUsbMount) {
		request.BlockUsbMount = &f.blockUsbMount
	}
	if flags.Changed(flagRemountUsbMode) {
		request.RemountUsbMode = &f.remountUsbMode
	}
	if flags.Changed(flagEventDetailURL) {
		request.EventDetailURL = &f.eventDetailURL
	}
	if flags.Changed(flagEventDetailText) {
		request.EventDetailText = &f.eventDetailText
	}
	return nil
}

// applyTo sets the settings whose flags were given in the config
func (f *santaSettingsFlags) applyTo(cmd *cobra.Command, config *machineconfiguration.MachineConfiguration) error {
	var request machineconfiguration.MachineConfigurationUpdateRequest
	if err := f.updateRequest(cmd, &request); err != nil {
		return err
	}
	config.EnableAllEventUpload = request.EnableAllEventUpload
	config.DisableUnknownEventUpload = request.DisableUnknownEventUpload
	if request.OverrideFileAccessAction != nil {
		config.OverrideFileAccessAction = *request.OverrideFileAccessAction
	}
	config.BlockUsbMount = request.BlockUsbMount
	if request.RemountUsbMode != nil {
		config.RemountUsbMode = *request.RemountUsbMode
	}
	config.EventDetailURL = request.EventDetailURL
	config.EventDetailText = request.EventDetailText
	return nil
}

func addConfirmGlobalLockdownFlag(cmd *cobra.Command, confirm *bool) {
	cmd.Flags().BoolVar(confirm, flagConfirmGlobalLockdown, false, "Confirm setting the global client mode to lockdown, which applies to every machine without a machine-specific configuration")
}

func globalConfigOptions(confirmGlobalLockdown bool) []machineconfiguration.GlobalConfigOption {
	if confirmGlobalLockdown {
		return []machineconfiguration.GlobalConfigOption{machineconfiguration.ConfirmGlobalLockdown()}
	}
	return nil
}

func wrapWriteError(err error) error {
	if errors.Is(err, machineconfiguration.ErrGlobalLockdownNotConfirmed) {
		return fmt.Errorf("%w; pass --%s to apply it", err, flagConfirmGlobalLockdown)
	}
	return fmt.Errorf("error writing the configuration to the sync server: %w", err)
}

// printSantaSettings prints the optional Santa settings of a config
func printSantaSettings(writer io.Writer, config machineconfiguration.MachineConfiguration) {
	fmt.Fprintln(writer, "EnableAllEventUpload:\t", boolSetting(config.EnableAllEventUpload))
	fmt.Fprintln(writer, "DisableUnknownEventUpload:\t", boolSetting(config.DisableUnknownEventUpload))
	fmt.Fprintln(writer, "OverrideFileAccessAction:\t", stringSetting(string(config.OverrideFileAccessAction), config.OverrideFileAccessAction != ""))
	fmt.Fprintln(writer, "BlockUsbMount:\t", boolSetting(config.BlockUsbMount))
	fmt.Fprintln(writer, "RemountUsbMode:\t", stringSetting(strings.Join(config.RemountUsbMode, ","), len(config.RemountUsbMode) > 0))
	fmt.Fprintln(writer, "EventDetailURL:\t", stringPtrSetting(config.EventDetailURL))
	fmt.Fprintln(writer, "EventDetailText:\t", stringPtrSetting(config.EventDetailText))
}

const notConfigured = "(not configured)"

func boolSetting(value *bool) string {
	if value == nil {
		return notConfigured
	}
	return fmt.Sprint(*value)
}

func stringSetting(value string, configured bool) string {
	if !configured {
		return notConfigured
	}
	return fmt.Sprintf("%q", value)
}

func stringPtrSetting(value *string) string {
	if value == nil {
		return notConfigured
	}
	return stringSetting(*value, true)
}
