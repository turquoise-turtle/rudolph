package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/spf13/cobra"
)

const (
	flagBlockedPaths              = "blocked-paths"
	flagAllowedPaths              = "allowed-paths"
	flagSkipRegexValidation       = "skip-regex-validation"
	flagConfirmGlobalLockdown     = "confirm-global-lockdown"
	flagEnableAllEventUpload      = "enable-all-event-upload"
	flagDisableUnknownEventUpload = "disable-unknown-event-upload"
	flagOverrideFileAccessAction  = "override-file-access-action"
	flagBlockUsbMount             = "block-usb-mount"
	flagRemountUsbMode            = "remount-usb-mode"
	flagEventDetailURL            = "event-detail-url"
	flagEventDetailText           = "event-detail-text"
	flagCleanSync                 = "clean-sync"
	flagInherit                   = "inherit"
)

// settingFlags maps the name of the flag of every setting a machine-specific configuration can override (as `config
// set` and `config update` name it) to the setting's attribute name, for --inherit
var settingFlags = map[string]string{
	"client-mode":                 "ClientMode",
	flagBlockedPaths:              "BlockedPathRegex",
	flagAllowedPaths:              "AllowedPathRegex",
	"batch-size":                  "BatchSize",
	"bundles":                     "EnableBundles",
	"transitive-rules":            "EnableTransitiveRules",
	"full-sync-interval":          "FullSyncInterval",
	"upload-logs":                 "UploadLogsUrl",
	flagEnableAllEventUpload:      "EnableAllEventUpload",
	flagDisableUnknownEventUpload: "DisableUnknownEventUpload",
	flagOverrideFileAccessAction:  "OverrideFileAccessAction",
	flagBlockUsbMount:             "BlockUsbMount",
	flagRemountUsbMode:            "RemountUsbMode",
	flagEventDetailURL:            "EventDetailURL",
	flagEventDetailText:           "EventDetailText",
}

func inheritableSettingFlags() []string {
	names := make([]string, 0, len(settingFlags))
	for name := range settingFlags {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// inheritedSettings converts the setting names given with --inherit to attribute names
func inheritedSettings(flagNames []string) ([]string, error) {
	var settings []string
	seen := map[string]bool{}
	for _, name := range flagNames {
		setting, ok := settingFlags[strings.TrimPrefix(name, "--")]
		if !ok {
			return nil, fmt.Errorf("--%s %q: unknown setting; must be one of %s", flagInherit, name, strings.Join(inheritableSettingFlags(), ", "))
		}
		if !seen[setting] {
			seen[setting] = true
			settings = append(settings, setting)
		}
	}
	return settings, nil
}

// combinePathRegexes combines several path regexes into one regex that matches a path if and only if any of
// them matches. Each pattern is validated with Go's regexp (RE2) syntax, which differs from the ICU syntax Santa
// uses (e.g. RE2 has no lookahead). Empty patterns are ignored, and no patterns result in "", which clears the
// setting on Santa.
func combinePathRegexes(patterns []string) (string, error) {
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return "", fmt.Errorf("invalid path regex %q (validated as Go RE2 syntax): %w", pattern, err)
		}
	}

	combined := combinePathRegexesUnchecked(patterns)
	if _, err := regexp.Compile(combined); err != nil {
		return "", fmt.Errorf("path regexes cannot be combined: %w", err)
	}
	return combined, nil
}

// combinePathRegexesUnchecked combines patterns like combinePathRegexes, without validating them.
//
// Santa prepends "^" to a regex that does not start with one and searches for it in the path, so a path regex
// always matches from the start of the path. A single pattern is returned unchanged. Several patterns are combined
// as ^(?:(?:p1)|(?:p2)...), which is anchored as a whole, so every pattern still only matches from the start.
func combinePathRegexesUnchecked(patterns []string) string {
	var groups []string
	for _, pattern := range patterns {
		if pattern != "" {
			groups = append(groups, pattern)
		}
	}

	switch len(groups) {
	case 0:
		return ""
	case 1:
		return groups[0]
	}
	for i, pattern := range groups {
		// Non-capturing groups keep each pattern's alternations to itself
		groups[i] = "(?:" + pattern + ")"
	}
	return "^(?:" + strings.Join(groups, "|") + ")"
}

// pathRegexFlags are the repeatable --blocked-paths and --allowed-paths flags
type pathRegexFlags struct {
	blocked        []string
	allowed        []string
	skipValidation bool
}

func (f *pathRegexFlags) addFlags(cmd *cobra.Command) {
	cmd.Flags().StringArrayVarP(&f.blocked, flagBlockedPaths, "b", nil, `A regex of paths to be blocked. Repeat the flag for several regexes (each value is one regex, commas included); a path is blocked if any of them matches. Santa matches regexes from the start of the path; wrap a single regex with "|" in a group, e.g. "^(?:a|b)"`)
	cmd.Flags().StringArrayVarP(&f.allowed, flagAllowedPaths, "a", nil, `A regex of paths to be allowed. Repeat the flag for several regexes (each value is one regex, commas included); a path is allowed if any of them matches. Santa matches regexes from the start of the path; wrap a single regex with "|" in a group, e.g. "^(?:a|b)"`)
	cmd.Flags().BoolVar(&f.skipValidation, flagSkipRegexValidation, false, `Do not validate path regexes. Validation uses Go's RE2 syntax, but Santa uses ICU regexes, so valid ICU syntax such as lookahead is rejected. Santa silently ignores a regex that is not valid ICU`)
}

func (f *pathRegexFlags) blockedPathRegex() (string, error) {
	return f.combine(flagBlockedPaths, f.blocked)
}

func (f *pathRegexFlags) allowedPathRegex() (string, error) {
	return f.combine(flagAllowedPaths, f.allowed)
}

func (f *pathRegexFlags) combine(flag string, patterns []string) (string, error) {
	warnUnanchoredAlternation(os.Stderr, flag, patterns)
	if f.skipValidation {
		return combinePathRegexesUnchecked(patterns), nil
	}
	regex, err := combinePathRegexes(patterns)
	if err != nil {
		return "", fmt.Errorf("--%s: %w (use --%s for ICU-only syntax)", flag, err, flagSkipRegexValidation)
	}
	return regex, nil
}

// warnUnanchoredAlternation warns when a single path regex has a top-level "|". It is sent unchanged, and Santa
// only prepends "^" to the whole regex, so only its first alternative is anchored to the start of the path.
// Several regexes are combined into a fully anchored regex, so they need no warning.
func warnUnanchoredAlternation(w io.Writer, flag string, patterns []string) {
	var nonEmpty []string
	for _, pattern := range patterns {
		if pattern != "" {
			nonEmpty = append(nonEmpty, pattern)
		}
	}
	if len(nonEmpty) != 1 || !hasTopLevelAlternation(nonEmpty[0]) {
		return
	}
	fmt.Fprintf(w, "Warning: --%s %q has a top-level \"|\": Santa only anchors its first alternative to the start of the path. "+
		"Wrap it in a group (e.g. \"^(?:a|b)\") or pass each alternative as a separate --%s flag.\n", flag, nonEmpty[0], flag)
}

// hasTopLevelAlternation reports whether pattern contains a "|" outside of groups, character classes, escapes and
// \Q...\E quotes
func hasTopLevelAlternation(pattern string) bool {
	depth := 0
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			if strings.HasPrefix(pattern[i:], `\Q`) {
				end := strings.Index(pattern[i+2:], `\E`)
				if end < 0 {
					return false
				}
				i += 2 + end + 1
			} else {
				i++
			}
		case '[':
			i = endOfCharacterClass(pattern, i)
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case '|':
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

// endOfCharacterClass returns the index of the "]" closing the character class opened at start (or the end of the
// pattern). A "]" right after "[" or "[^" is a literal, and classes may nest (ICU sets such as [a-z[0-9]]).
func endOfCharacterClass(pattern string, start int) int {
	depth := 0
	for i := start; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			i++
		case '[':
			depth++
			if strings.HasPrefix(pattern[i+1:], "^") {
				i++
			}
			if strings.HasPrefix(pattern[i+1:], "]") {
				i++
			}
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return len(pattern)
}

// optionalClientMode is a --client-mode flag without a default: it is only applied when given
type optionalClientMode struct {
	mode flags.ClientMode
	set  bool
}

func (o *optionalClientMode) Set(value string) error {
	if err := o.mode.Set(value); err != nil {
		return err
	}
	o.set = true
	return nil
}

func (o *optionalClientMode) Type() string {
	return o.mode.Type()
}

func (o *optionalClientMode) String() string {
	if !o.set {
		return ""
	}
	return o.mode.String()
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
	cmd.Flags().StringSliceVar(&f.remountUsbMode, flagRemountUsbMode, nil, `Mount flags (e.g. "rdonly,noexec") to remount blocked USB mass storage devices with. Requires USB mounts to be blocked (--block-usb-mount, or inherited from the global configuration for a machine)`)
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
	cmd.Flags().BoolVar(confirm, flagConfirmGlobalLockdown, false, "Confirm setting the global client mode to a mode that blocks unknown executions (lockdown or standalone), which applies to every machine without a machine-specific configuration")
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

// inheritedSettingText formats the global value of a setting (named by its attribute) as it is shown when it is set
func inheritedSettingText(global machineconfiguration.MachineConfiguration, setting string) string {
	switch setting {
	case "ClientMode":
		clientModeText, err := global.ClientMode.MarshalText()
		if err != nil {
			return fmt.Sprintf("%d -->( INVALID, not sent to Santa )", global.ClientMode)
		}
		return fmt.Sprintf("%d -->( %s )", global.ClientMode, clientModeText)
	case "BlockedPathRegex":
		return fmt.Sprintf("%q", global.BlockedPathRegex)
	case "AllowedPathRegex":
		return fmt.Sprintf("%q", global.AllowedPathRegex)
	case "BatchSize":
		return fmt.Sprint(global.BatchSize)
	case "EnableBundles":
		return fmt.Sprint(global.EnableBundles)
	case "EnableTransitiveRules":
		return fmt.Sprint(global.EnabledTransitiveRules)
	case "FullSyncInterval":
		if global.FullSyncInterval == 0 {
			return notConfigured
		}
		return fmt.Sprint(global.FullSyncInterval)
	case "UploadLogsUrl":
		return stringSetting(global.UploadLogsURL, global.UploadLogsURL != "")
	case "EnableAllEventUpload":
		return boolSetting(global.EnableAllEventUpload)
	case "DisableUnknownEventUpload":
		return boolSetting(global.DisableUnknownEventUpload)
	case "OverrideFileAccessAction":
		return stringSetting(string(global.OverrideFileAccessAction), global.OverrideFileAccessAction != "")
	case "BlockUsbMount":
		return boolSetting(global.BlockUsbMount)
	case "RemountUsbMode":
		return stringSetting(strings.Join(global.RemountUsbMode, ","), global.RemountUsbMode != nil)
	case "EventDetailURL":
		return stringPtrSetting(global.EventDetailURL)
	case "EventDetailText":
		return stringPtrSetting(global.EventDetailText)
	}
	return ""
}

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
