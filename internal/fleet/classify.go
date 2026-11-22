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
	// hashWindow limits the outlier check to Macs that completed a sync recently enough to have the current rules.
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

	// ParseSantaVersion reads an empty or unparseable version as "current"; here that is unknown, never old.
	if v := rudolphtypes.ParseSantaVersion(m.sensor.SantaVersion); v.IsCurrent() {
		r.versionUnknown = true
	} else if v.Before(s.minVersion) {
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
	return hashSample{
		id: m.id, hostname: m.sensor.Hostname, serial: m.sensor.SerialNum, lastSeen: m.sensor.Time,
		hash: m.state.RulesHashAfterSync, postflight: post,
		received: m.state.RulesReceived, processed: m.state.RulesProcessed, listed: r.notApplied,
	}, true
}

// outlier is a Mac whose rules hash differs from the one most Macs with the same rules report.
type outlier struct {
	hashSample
	expected string
}

// settleOutliers finds the hash outliers. Macs with the same MachineRulesTracked digest are sent the same rules, so
// after a completed sync they should report the same hash. In each group of at least three such Macs, the majority
// hash is the one more than half report; a Mac is an outlier when its hash differs and its postflight is more than
// syncstate.FeedSyncCursorOverlap after the earliest postflight that produced the majority hash, so a Mac that has
// simply not synced since the rules last changed is not one. A Mac already listed under Rules is not listed twice.
func settleOutliers(samples map[string][]hashSample) []outlier {
	var found []outlier
	for _, group := range samples {
		if len(group) < 3 {
			continue
		}
		counts := map[string]int{}
		for _, s := range group {
			counts[s.hash]++
		}
		majority := ""
		for hash, n := range counts {
			if 2*n > len(group) {
				majority = hash
			}
		}
		if majority == "" {
			continue
		}
		var earliest time.Time
		for _, s := range group {
			if s.hash == majority && (earliest.IsZero() || s.postflight.Before(earliest)) {
				earliest = s.postflight
			}
		}
		for _, s := range group {
			if s.hash != majority && !s.listed && s.postflight.Sub(earliest) > syncstate.FeedSyncCursorOverlap {
				found = append(found, outlier{hashSample: s, expected: majority})
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
	return item
}
