package fleet

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/groups"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/model/sensordata"
	"github.com/airbnb/rudolph/pkg/model/syncstate"
	rudolphtypes "github.com/airbnb/rudolph/pkg/types"
)

// The health categories, as the lists and the pointer's Health map name them.
const (
	categoryStale      = "Stale"
	categoryIncomplete = "Incomplete"
	categoryMode       = "Mode"
	categoryRules      = "Rules"
	categorySIP        = "SIP"
	categoryVersion    = "Version"
)

var categories = []string{categoryStale, categoryIncomplete, categoryMode, categoryRules, categorySIP, categoryVersion}

// The readiness buckets: the first character of a readiness row's sort key, so a list reads best first and the web
// view pages one bucket as a key range.
const (
	bucketReady      = "0"
	bucketFew        = "1"
	bucketMany       = "2"
	bucketUnmeasured = "3"
	bucketLocked     = "4"

	// manyPending is the pending count from which a Mac is Many rather than Few.
	manyPending = 10
	// observedDays is how long a Mac's unknown binaries must have been recorded before "none pending" means anything:
	// the window the figures cover.
	observedDays = 30

	scopeAll = "All"

	reasonNotApplied = "NotApplied"
	reasonOutlier    = "Outlier"

	// postflightGrace is how long after its preflight a sync without a postflight is still simply in progress; the
	// web view's machine page uses the same 30 minutes.
	postflightGrace = 30 * time.Minute
	// hashWindow limits the outlier check to Macs that completed a sync recently: an older hash says little about the
	// Mac now. It does not by itself mean the Mac has the current rules; the newest rule change of the Mac's set
	// decides that (ruleChanges.of, settleOutliers).
	hashWindow = 24 * time.Hour

	orderLayout = "20060102T150405Z"
	noOrder     = "00000000T000000Z"
)

// machine is one Mac as the store table holds it.
type machine struct {
	id         string
	sensor     *sensordata.SensorData
	state      *syncstate.SyncStateRow
	configItem map[string]types.AttributeValue
	groups     *groups.MachineGroups

	undecodable bool
}

// settings are what every Mac of a run is judged against.
type settings struct {
	now        time.Time
	today      time.Time
	staleAfter time.Duration
	minVersion rudolphtypes.SantaVersion
	// versionOff is the minimum Santa version "off": no Mac is listed under Version.
	versionOff bool
	registry   *groups.Registry
	global     *machineconfiguration.MachineConfiguration
}

// report is what the job concludes about one Mac, before its unknown markers are read.
type report struct {
	// lastCompleted is the Mac's last completed sync; zero when there is none on record.
	lastCompleted time.Time
	preflightAt   time.Time

	stale, incomplete, mode, notApplied, sip, version bool
	versionUnknown                                    bool
	versionOrder                                      string

	// cleanSyncRequested is the config's CleanSync: a clean sync is requested, by anyone, and not yet performed.
	// autoCleanSyncFor and autoCleanSyncAt are the fleet job's last request for the Mac; both zero when there is
	// none, or when its time cannot be read.
	cleanSyncRequested bool
	autoCleanSyncFor   string
	autoCleanSyncAt    time.Time

	intended     rudolphtypes.ClientMode
	source       string
	groups       []string
	locked       bool
	why          string
	observedDays int
}

// classify works out a Mac's health categories and what readiness needs, from its stored rows alone.
func classify(m *machine, s settings) report {
	var r report

	// Last completed sync: this sync's postflight, else the newest earlier sync that completed.
	if m.state != nil {
		r.preflightAt, _ = clock.ParseRFC3339(m.state.PreflightAt)
		if t, err := clock.ParseRFC3339(m.state.PostflightAt); err == nil {
			r.lastCompleted = t
		} else {
			for _, c := range m.state.CompletedSyncs {
				if t, err := clock.ParseRFC3339(c.PreflightAt); err == nil && t.After(r.lastCompleted) {
					r.lastCompleted = t
				}
			}
		}
	}
	r.stale = r.lastCompleted.IsZero() || s.now.Sub(r.lastCompleted) > s.staleAfter
	// Reaching preflight but not finishing. A preflight older than the stale threshold is no longer "talking to
	// Rudolph": that Mac is Stale only.
	if m.state != nil && m.state.PostflightAt == "" && !r.preflightAt.IsZero() {
		age := s.now.Sub(r.preflightAt)
		r.incomplete = age >= postflightGrace && age <= s.staleAfter
	}

	// The mode Rudolph intends now, resolved with preflight's own functions.
	config, ownMode, err := machineconfiguration.IntendedFromStored(s.global, m.configItem)
	if err != nil {
		// Only when the global config cannot be encoded again; the Mac then inherits nothing readable.
		config = machineconfiguration.GetUniversalDefaultConfig()
	}
	r.cleanSyncRequested = config.CleanSync
	// A request whose time cannot be read (edited by hand) counts as none: the next request writes a readable one,
	// where keeping it would leave the Mac neither listed nor requested for as long as the rules do not change.
	if at, err := clock.ParseRFC3339(config.AutoCleanSyncAt); err == nil {
		r.autoCleanSyncFor, r.autoCleanSyncAt = config.AutoCleanSyncFor, at
	}
	groupMode, group, hasGroupMode := groups.MachineClientMode(s.registry, m.groups)
	intended, source := machineconfiguration.IntendedClientMode(config, ownMode, s.global, groupMode, hasGroupMode, s.now)
	r.intended, r.source = intended, string(source)
	if source == machineconfiguration.ClientModeFromGroup {
		r.source += ":" + group
	}
	// A stale Mac's reported mode is old news, not a mismatch.
	r.mode = !r.stale && m.sensor.ClientMode != intended

	r.notApplied = m.state != nil && m.state.PostflightAt != "" && m.state.RulesProcessed < m.state.RulesReceived
	// SIPStatus holds the CSR flags: absent or 0 is SIP fully on.
	r.sip = m.sensor.SIPStatus != nil && *m.sensor.SIPStatus != 0

	// ParseSantaVersion reads an empty or unparseable version as "current"; here that is unknown, never old. It is
	// counted with the check off too: it says the Mac's data is poor, not that its release is below the minimum.
	if v := rudolphtypes.ParseSantaVersion(m.sensor.SantaVersion); v.IsCurrent() {
		r.versionUnknown = true
	} else if !s.versionOff && v.Before(s.minVersion) {
		r.version = true
		r.versionOrder = fmt.Sprintf("%04d.%02d", v.Year, v.Month)
	}

	r.groups = groups.Effective(s.registry, m.groups)
	r.locked = intended == rudolphtypes.Lockdown || intended == rudolphtypes.Standalone
	if m.state != nil {
		if first, err := time.Parse(dayLayout, m.state.FirstSeenDay); err == nil && !first.After(s.today) {
			r.observedDays = int(s.today.Sub(first).Hours() / 24)
		}
	}
	// Why a Mac with nothing pending is still not known to be ready: every case in which Santa uploads no unknown
	// executions, or has not been watched for the whole window, looks exactly like a Mac that ran nothing unknown.
	switch {
	case r.stale:
		r.why = "stale"
	case r.incomplete:
		r.why = "incomplete"
	case config.DisableUnknownEventUpload != nil && *config.DisableUnknownEventUpload:
		r.why = "upload-off"
	case reportsNoRules(m.sensor):
		// Santa uploads no events during a clean sync, which a Mac without rules gets on every sync.
		r.why = "no-rules"
	case r.observedDays < observedDays:
		r.why = "observed"
	}
	return r
}

func reportsNoRules(s *sensordata.SensorData) bool {
	return s.RuleCount == 0 && s.BinaryRuleCount == 0 && s.CertificateRuleCount == 0 && s.CDHashRuleCount == 0 &&
		s.TeamIDRuleCount == 0 && s.SigningIDRuleCount == 0 && s.CompilerRuleCount == 0 && s.TransitiveRuleCount == 0
}

// bucket is the Mac's readiness bucket, first match: Locked, Unmeasured, Many, Few, Ready.
func (r report) bucket(u unknownCounts) string {
	switch {
	case r.locked:
		return bucketLocked
	case r.why != "":
		return bucketUnmeasured
	case u.capped || u.pending30 >= manyPending:
		// A capped Mac has more unknown binaries than were read, so what was read cannot clear it.
		return bucketMany
	case u.pending30 > 0:
		return bucketFew
	}
	return bucketReady
}

// categories returns the health categories the Mac is listed in during the walk (a hash outlier is added afterwards).
func (r report) categories() []string {
	var listed []string
	for category, in := range map[string]bool{
		categoryStale: r.stale, categoryIncomplete: r.incomplete, categoryMode: r.mode,
		categoryRules: r.notApplied, categorySIP: r.sip, categoryVersion: r.version,
	} {
		if in {
			listed = append(listed, category)
		}
	}
	return listed
}

func modeName(mode rudolphtypes.ClientMode) string {
	if text, err := mode.MarshalText(); err == nil {
		return string(text)
	}
	return strconv.Itoa(int(mode))
}

// base is what every row says about its Mac. Hostname and serial number repeat the store table's, so a list needs no
// read per row; they are left out when the Mac reports none.
func (r *run) base(id, hostname, serial string) map[string]types.AttributeValue {
	item := map[string]types.AttributeValue{"MachineID": str(id), "ExpiresAfter": r.expires}
	if hostname != "" {
		item["Hostname"] = str(hostname)
	}
	if serial != "" {
		item["SerialNum"] = str(serial)
	}
	return item
}

func (r *run) healthRow(category, order string, m *machine, rep report) map[string]types.AttributeValue {
	item := r.base(m.id, m.sensor.Hostname, m.sensor.SerialNum)
	item["PK"] = str(snapshotPKPrefix + r.snapshot + "#Health#" + category)
	item["SK"] = str(m.id)
	if order != "" {
		item["SK"] = str(order + "#" + m.id)
	}
	if m.sensor.Time != "" {
		item["LastSeen"] = str(m.sensor.Time)
	}
	if !rep.lastCompleted.IsZero() {
		item["LastCompleted"] = str(rep.lastCompleted.UTC().Format(time.RFC3339))
	}
	return item
}

// healthRows builds the Mac's row for each category it is in.
func (r *run) healthRows(m *machine, rep report) []map[string]types.AttributeValue {
	var rows []map[string]types.AttributeValue
	never := &types.AttributeValueMemberBOOL{Value: rep.lastCompleted.IsZero()}
	if rep.stale {
		// Oldest first, a Mac that never completed a sync before any other
		order := noOrder
		if !rep.lastCompleted.IsZero() {
			order = rep.lastCompleted.UTC().Format(orderLayout)
		}
		row := r.healthRow(categoryStale, order, m, rep)
		row["Never"] = never
		rows = append(rows, row)
	}
	if rep.incomplete {
		row := r.healthRow(categoryIncomplete, rep.preflightAt.UTC().Format(orderLayout), m, rep)
		row["SyncsWithoutPostflight"] = num(m.state.SyncsWithoutPostflight)
		row["Never"] = never
		rows = append(rows, row)
	}
	if rep.mode {
		row := r.healthRow(categoryMode, "", m, rep)
		row["Reported"] = str(modeName(m.sensor.ClientMode))
		row["Intended"] = str(modeName(rep.intended))
		row["Source"] = str(rep.source)
		rows = append(rows, row)
	}
	if rep.notApplied {
		row := r.healthRow(categoryRules, "", m, rep)
		row["Reason"] = str(reasonNotApplied)
		row["RulesReceived"] = num(m.state.RulesReceived)
		row["RulesProcessed"] = num(m.state.RulesProcessed)
		if m.state.RulesHashAfterSync != "" {
			row["Hash"] = str(m.state.RulesHashAfterSync)
		}
		rows = append(rows, row)
	}
	if rep.sip {
		row := r.healthRow(categorySIP, "", m, rep)
		row["Flags"] = str(fmt.Sprintf("%#x", *m.sensor.SIPStatus))
		rows = append(rows, row)
	}
	if rep.version {
		row := r.healthRow(categoryVersion, rep.versionOrder, m, rep)
		row["SantaVersion"] = str(m.sensor.SantaVersion)
		rows = append(rows, row)
	}
	return rows
}

// readinessRow builds the Mac's readiness row, without its partition key: the same row goes in the fleet's list and
// in each of its groups'. The sort key puts the fewest pending first within a bucket.
func (r *run) readinessRow(m *machine, rep report, u unknownCounts, bucket string) map[string]types.AttributeValue {
	item := r.base(m.id, m.sensor.Hostname, m.sensor.SerialNum)
	item["SK"] = str(fmt.Sprintf("%s#%06d#%s", bucket, u.pending30, m.id))
	item["Capped"] = &types.AttributeValueMemberBOOL{Value: u.capped}
	item["ReportedMode"] = str(modeName(m.sensor.ClientMode))
	item["IntendedMode"] = str(modeName(rep.intended))
	item["ObservedDays"] = num(rep.observedDays)
	if bucket != bucketLocked {
		// A locked Mac's markers were not read: no figure is better than a zero that looks measured.
		item["Unknown30"] = num(u.unknown30)
		item["Pending30"] = num(u.pending30)
		item["Unknown7"] = num(u.unknown7)
		item["Pending7"] = num(u.pending7)
	}
	if bucket == bucketUnmeasured {
		item["Why"] = str(rep.why)
	}
	return item
}

// hashSample is what the outlier check keeps of a Mac that completed its current sync in the last day, and enough to
// write its row should it turn out to be an outlier: a run holds one for most Macs, so it is kept small.
type hashSample struct {
	id, hostname, serial, lastSeen string
	hash                           string
	postflight                     time.Time
	received, processed            int
	// attention is set when the Mac is already in a health category; listed is set when that category is Rules.
	attention, listed bool
	// What requestCleanSyncs decides on, should the Mac be an outlier: whether a clean sync is requested for it now,
	// the rule change the fleet job last requested one for, and whether a clean sync completed after that request.
	cleanSyncRequested, cleanSyncSince bool
	autoCleanSyncFor                   string
}

// hashSample returns the Mac's sample, and false when it cannot be judged: no completed sync in the last day, no
// hash reported, or a rules digest that says nothing about what the Mac was sent (empty, or one of the dirty values).
func (r report) hashSample(m *machine, now time.Time) (hashSample, bool) {
	if m.state == nil || m.state.PostflightAt == "" || m.state.RulesHashAfterSync == "" {
		return hashSample{}, false
	}
	digest := m.state.MachineRulesTracked
	if digest == "" || strings.HasPrefix(digest, syncstate.MachineRulesTrackedDirty) {
		return hashSample{}, false
	}
	post, err := clock.ParseRFC3339(m.state.PostflightAt)
	if err != nil || now.Sub(post) > hashWindow {
		return hashSample{}, false
	}
	// LastCompletedCleanSync is the start of that clean sync, so one already under way when the request was made does
	// not count: its hash would be read as "still differs" although the request may yet be performed.
	cleanSyncSince := false
	if r.autoCleanSyncFor != "" {
		if last, err := clock.ParseRFC3339(m.state.LastCompletedCleanSync()); err == nil {
			cleanSyncSince = last.After(r.autoCleanSyncAt)
		}
	}
	return hashSample{
		id: m.id, hostname: m.sensor.Hostname, serial: m.sensor.SerialNum, lastSeen: m.sensor.Time,
		hash: m.state.RulesHashAfterSync, postflight: post,
		received: m.state.RulesReceived, processed: m.state.RulesProcessed, listed: r.notApplied,
		cleanSyncRequested: r.cleanSyncRequested, cleanSyncSince: cleanSyncSince, autoCleanSyncFor: r.autoCleanSyncFor,
	}, true
}

// outlier is a Mac whose rules hash differs from the one most Macs in its ruleSet report.
type outlier struct {
	hashSample
	expected string
	// ruleChange is the newest rule change of the Mac's set (ruleChanges.of); zero when none is on record.
	ruleChange time.Time
	// afterCleanSync is set by requestCleanSyncs: the hash still differs after the clean sync the job requested.
	afterCleanSync bool
}

// ruleSet names the Macs whose hashes can be compared: those sent the same rules. The digest alone does not say that.
// MachineRulesTracked covers the sort keys of the machine and group rules a Mac is sent, not their policies, so two
// groups with a rule for the same identifier and different policies give their Macs one digest and two hashes; the
// larger group's hash would then be "expected" of the smaller group's Macs. The effective groups, as worked out for
// the readiness scopes, tell those apart.
type ruleSet struct {
	digest string
	// groups is the Mac's effective groups, sorted and joined with groupSeparator.
	groups string
}

// groupSeparator cannot occur in a group name.
const groupSeparator = "\x00"

func groupsKey(effective []string) string { return strings.Join(effective, groupSeparator) }

// ruleChanges is when the rules Macs are sent last changed, as far as the store records it.
type ruleChanges struct {
	// feed is the stamp of the newest rules feed row, the newest global rule change; zero when the feed has no row.
	feed time.Time
	// groups holds each group's RulesChangedAt. A group without one is not in the map and counts as never changed:
	// it was registered before the stamp existed and its rules have not changed since, or it has never had a rule.
	groups map[string]time.Time
	// unreadable holds the groups whose RulesChangedAt is there but is not a time.
	unreadable map[string]bool
}

// of returns the newest change to the rules of the Macs in set: the latest of the newest global rule change and the
// last rule change of each of the set's groups. Zero means no change on record.
//
// judge is false when one of the set's groups has a RulesChangedAt that cannot be read, which only an entry edited by
// hand has. The set is then left out of this run, for the reason lastRuleChange skips the whole check on an
// unreadable feed row: reading it as "no change" would compare Macs that synced before a real change with those that
// synced after it, and list the ones that are up to date. Other sets are not affected.
func (c ruleChanges) of(set ruleSet) (at time.Time, judge bool) {
	at = c.feed
	for _, group := range strings.Split(set.groups, groupSeparator) {
		if c.unreadable[group] {
			return time.Time{}, false
		}
		if changed := c.groups[group]; changed.After(at) {
			at = changed
		}
	}
	return at, true
}

// settleOutliers finds the hash outliers. Macs in the same ruleSet (the same MachineRulesTracked digest and the same
// effective groups) are sent the same rules, so after a completed sync they should report the same hash. Only Macs
// whose postflight is more than syncstate.FeedSyncCursorOverlap after the set's newest rule change (ruleChanges.of)
// take part, in the count and as outliers alike: a Mac that has not synced since then reports the hash of the rules
// before the change, and when such Macs are the majority (a rule changed late in the day, most laptops shut) counting
// them would make the stale hash the expected one and list every Mac that is up to date. Within the overlap a sync
// may or may not have been sent a global change, and a group's stamp is the clock of whoever wrote the rule, which
// may be a few minutes off. When no change is on record for a set, every sample in it takes part.
//
// In each set of at least three Macs that take part, the majority hash is the one more than half report, and a Mac
// reporting another is an outlier. A Mac already listed under Rules is not listed twice.
//
// What this still cannot tell apart: two Macs in the same groups with machine rules for the same identifier and
// different policies share a ruleSet, and a machine rule replaced in place leaves no stamp.
func settleOutliers(samples map[ruleSet][]hashSample, changes ruleChanges) []outlier {
	var found []outlier
	for set, all := range samples {
		ruleChange, judge := changes.of(set)
		if !judge {
			continue
		}
		taking := all
		if !ruleChange.IsZero() {
			taking = nil
			for _, s := range all {
				if s.postflight.Sub(ruleChange) > syncstate.FeedSyncCursorOverlap {
					taking = append(taking, s)
				}
			}
		}
		if len(taking) < 3 {
			continue
		}
		counts := map[string]int{}
		for _, s := range taking {
			counts[s.hash]++
		}
		majority := ""
		for hash, n := range counts {
			if 2*n > len(taking) {
				majority = hash
			}
		}
		if majority == "" {
			continue
		}
		for _, s := range taking {
			if s.hash != majority && !s.listed {
				found = append(found, outlier{hashSample: s, expected: majority, ruleChange: ruleChange})
			}
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].id < found[j].id })
	return found
}

func (r *run) outlierRow(o outlier) map[string]types.AttributeValue {
	item := r.base(o.id, o.hostname, o.serial)
	item["PK"] = str(snapshotPKPrefix + r.snapshot + "#Health#" + categoryRules)
	item["SK"] = str(o.id)
	if o.lastSeen != "" {
		item["LastSeen"] = str(o.lastSeen)
	}
	item["LastCompleted"] = str(o.postflight.UTC().Format(time.RFC3339))
	item["Reason"] = str(reasonOutlier)
	item["RulesReceived"] = num(o.received)
	item["RulesProcessed"] = num(o.processed)
	item["Hash"] = str(o.hash)
	item["ExpectedHash"] = str(o.expected)
	if o.afterCleanSync {
		item["AfterCleanSync"] = &types.AttributeValueMemberBOOL{Value: true}
	}
	return item
}
