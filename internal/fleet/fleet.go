// Package fleet builds the web view's fleet health lists and Lockdown readiness figures (docs/fleet-health.md). Once a
// day, as its own Lambda (cmd/fleet), it walks every Mac, works out which health categories it is in and how ready it
// is for Lockdown, and writes a snapshot to the events table: list rows first, then one pointer holding every count. A
// page view then reads the pointer and one bounded Query, never the fleet.
//
// It publishes no metrics and sends no notifications. Its one alarm is built from Lambda's own Invocations and Errors,
// so a run that cannot finish must return an error.
package fleet

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/airbnb/rudolph/internal/snapshot"
	"github.com/airbnb/rudolph/pkg/counterfields"
	"github.com/airbnb/rudolph/pkg/model/feedrules"
	"github.com/airbnb/rudolph/pkg/model/fleethealth"
	"github.com/airbnb/rudolph/pkg/model/groups"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/model/sensordata"
	"github.com/airbnb/rudolph/pkg/model/syncstate"
	rudolphtypes "github.com/airbnb/rudolph/pkg/types"
)

const (
	// DefaultMinSantaVersion is the release below which a Mac is listed under Version when the fleet health setting
	// stores none. An alias: the one default is fleethealth's.
	DefaultMinSantaVersion = fleethealth.DefaultMinSantaVersion

	// DefaultMaxMacs bounds a run. About two reads and one write call a Mac, 32 Macs at a time, fit 250,000 Macs in
	// the Lambda's 900 seconds; past that the walk would have to be split across invocations. A run that finds more
	// fails before writing anything, and the job's alarm fires.
	DefaultMaxMacs = 250_000

	// Written by the event writer (internal/handlers/eventupload).
	unknownMarkerPrefix = "UnknownSeen#"

	snapshotPKPrefix = "FleetSnapshot#"
	pointerPK        = "FleetPointer"
	pointerSK        = "Current"

	// The store table's index of every sensor data row, the only way to list every Mac without a Scan.
	machineIndex = "DataType_MachineID"
	// The store table's partition of global rules (pkg/model/globalrules).
	globalRulesPK = "GlobalRules"

	// Rows outlive the next few daily runs, so a reader paging a list keeps finding its rows after a newer run, and a
	// job that stops leaves a pointer whose rows are gone within days: the web view then says the lists were not
	// computed since GeneratedAt. DynamoDB's TTL deletes the rows for free.
	snapshotTTL = 3 * 24 * time.Hour

	// markerLimit is one more than the unknown binaries counted for a Mac. A Mac's marker partition is read with one
	// Query and no further, whatever a Mac (or a holder of the sync token) uploaded: 1,001 rows mean "more than 1,000".
	markerLimit = 1001

	// parallelism bounds the Macs in work at once, each making one call at a time.
	parallelism = 32

	// writeRowsPerSecond paces the snapshot's rows. Every Mac has a row in the one Ready#All partition, and DynamoDB
	// serves about 1,000 write units a second to a partition; unpaced, a large fleet's rows would be throttled, and
	// would take the events table's capacity from eventupload.
	writeRowsPerSecond = 1000

	dayLayout = "2006-01-02"
)

// Client is the part of *dynamodb.Client the job uses. It never scans.
type Client interface {
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	Query(ctx context.Context, in *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	BatchWriteItem(ctx context.Context, in *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error)
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
}

type Options struct {
	Client Client
	// Table is the events table (EVENTS_DYNAMODB_NAME): unknown markers are read from it, the snapshot is written to it.
	Table string
	// StoreTable is the store table (DYNAMODB_NAME): Macs, configuration, groups, global rules and the rules feed.
	StoreTable string
	// Now defaults to time.Now.
	Now func() time.Time
	// MaxMacs defaults to DefaultMaxMacs.
	MaxMacs int
}

// run is one run's fixed inputs and running totals.
type run struct {
	opts     Options
	settings settings
	snapshot string
	expires  types.AttributeValue
	pacer    pacer

	// The global rule sort keys, read on the first Mac that has an unknown binary to check: a fleet already in
	// Lockdown, or one with nothing unknown, never reads them.
	rulesOnce sync.Once
	rules     map[string]struct{}
	rulesErr  error

	mu      sync.Mutex
	totals  totals
	samples map[string][]hashSample
	// interned keeps one copy of each rules hash and digest: Macs sent the same rules report the same ones.
	interned map[string]string
}

// totals is what the pointer holds.
type totals struct {
	macs, attention, versionUnknown, capped int
	health                                  map[string]int
	readiness                               map[string]*readinessCounts
}

type readinessCounts struct{ Macs, Ready, Few, Many, Unmeasured, Locked int }

// Run builds one snapshot and moves the pointer to it. Rows are written before the pointer, so a reader never sees a
// half-written snapshot; a failed run leaves the pointer on the previous snapshot and returns an error.
func Run(ctx context.Context, opts Options) error {
	if opts.Client == nil || opts.Table == "" || opts.StoreTable == "" {
		return errors.New("fleet: Client, Table and StoreTable are required")
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn().UTC()
	started := time.Now()
	if opts.MaxMacs <= 0 {
		opts.MaxMacs = DefaultMaxMacs
	}

	r := &run{opts: opts, samples: map[string][]hashSample{}, interned: map[string]string{}}
	r.pacer.perRow = time.Second / writeRowsPerSecond
	r.expires = &types.AttributeValueMemberN{Value: strconv.FormatInt(now.Add(snapshotTTL).Unix(), 10)}

	healthItem, err := r.getStoreItem(ctx, fleethealth.PartitionKey, fleethealth.SortKey)
	if err != nil {
		return err
	}
	registryKey := groups.RegistryKey()
	registryItem, err := r.getStoreItem(ctx, registryKey.PartitionKey, registryKey.SortKey)
	if err != nil {
		return err
	}
	registry, err := groups.RegistryFromItem(registryItem)
	if err != nil {
		return fmt.Errorf("fleet: %w", err)
	}
	globalItem, err := r.getStoreItem(ctx, machineconfiguration.GlobalConfigPartitionKey, machineconfiguration.ConfigSortKey)
	if err != nil {
		return err
	}
	health := fleethealth.FromItem(healthItem)
	staleDays, holiday := health.InForce(now)
	// The minimum has no option or environment variable: a run must not disagree with what the CLI and the web view
	// show as in force. The pointer records it, so a reader knows which minimum the lists on show were built with.
	minSantaVersion, versionOff, _ := health.MinSantaVersionInForce()
	r.settings = settings{
		now:        now,
		today:      time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC),
		staleAfter: time.Duration(staleDays) * 24 * time.Hour,
		minVersion: rudolphtypes.ParseSantaVersion(minSantaVersion),
		versionOff: versionOff,
		registry:   registry,
		global:     machineconfiguration.StoredGlobalConfig(globalItem),
	}

	r.totals.health = map[string]int{}
	for _, category := range categories {
		r.totals.health[category] = 0
	}
	r.totals.readiness = map[string]*readinessCounts{scopeAll: {}, groups.Ungrouped: {}}
	for name := range registry.Groups {
		r.totals.readiness[name] = &readinessCounts{}
	}

	ids, err := r.machineIDs(ctx)
	if err != nil {
		return err
	}
	if r.snapshot, err = snapshot.ID(now); err != nil {
		return err
	}

	err = snapshot.Parallel(ctx, parallelism, len(ids), func(ctx context.Context, i int) error {
		if err := r.machine(ctx, ids[i]); err != nil {
			return fmt.Errorf("fleet: machine %s: %w", ids[i], err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	ids = nil

	// Whether a Mac's rules hash is an outlier is only known once every Mac with the same rules has been seen. The
	// rule change is read after the walk, not before: a change made during the walk is then seen, and the Macs that
	// synced before it are left out rather than judged against it.
	ruleChange, judge, err := r.lastRuleChange(ctx)
	if err != nil {
		return err
	}
	var settled []outlier
	if judge {
		settled = settleOutliers(r.samples, ruleChange)
	}
	var outliers []types.WriteRequest
	for _, o := range settled {
		outliers = append(outliers, types.WriteRequest{PutRequest: &types.PutRequest{Item: r.outlierRow(o)}})
		r.totals.health[categoryRules]++
		if !o.attention {
			r.totals.attention++
		}
	}
	if err := r.write(ctx, outliers); err != nil {
		return err
	}

	pointer := map[string]types.AttributeValue{
		"PK":              str(pointerPK),
		"SK":              str(pointerSK),
		"Snapshot":        str(r.snapshot),
		"GeneratedAt":     str(now.Format(time.RFC3339)),
		"Macs":            num(r.totals.macs),
		"Attention":       num(r.totals.attention),
		"VersionUnknown":  num(r.totals.versionUnknown),
		"MinSantaVersion": str(minSantaVersion),
		"StaleDays":       num(staleDays),
		"Holiday":         &types.AttributeValueMemberBOOL{Value: holiday},
		"Capped":          num(r.totals.capped),
	}
	if holiday {
		pointer["HolidayUntil"] = str(health.HolidayUntil)
	}
	counts := map[string]types.AttributeValue{}
	for category, n := range r.totals.health {
		counts[category] = num(n)
	}
	pointer["Health"] = &types.AttributeValueMemberM{Value: counts}
	readiness := map[string]types.AttributeValue{}
	for scope, c := range r.totals.readiness {
		av, err := attributevalue.MarshalMap(c)
		if err != nil {
			return err
		}
		readiness[scope] = &types.AttributeValueMemberM{Value: av}
	}
	pointer["Readiness"] = &types.AttributeValueMemberM{Value: readiness}

	moved, err := snapshot.MovePointer(ctx, opts.Client, opts.Table, pointer, r.snapshot)
	if err != nil {
		return fmt.Errorf("fleet: pointer: %w", err)
	}
	if !moved {
		log.Printf("fleet: the pointer already names a newer snapshot; left as it is")
	}
	// The figures to watch against the Lambda's memory and timeout: docs/fleet-health.md, "Limits".
	all := r.totals.readiness[scopeAll]
	log.Printf("fleet: macs=%d rules=%d attention=%d stale=%d incomplete=%d ready=%d locked=%d stale_days=%d holiday=%t min_santa_version=%s seconds=%.1f",
		r.totals.macs, len(r.rules), r.totals.attention, r.totals.health[categoryStale], r.totals.health[categoryIncomplete],
		all.Ready, all.Locked, staleDays, holiday, minSantaVersion, time.Since(started).Seconds())
	return nil
}

func (r *run) getStoreItem(ctx context.Context, pk, sk string) (map[string]types.AttributeValue, error) {
	out, err := r.opts.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(r.opts.StoreTable),
		Key:       map[string]types.AttributeValue{"PK": str(pk), "SK": str(sk)},
	})
	if err != nil {
		return nil, fmt.Errorf("fleet: %s / %s: %w", pk, sk, err)
	}
	return out.Item, nil
}

// lastRuleChange returns when a global rule was last added, changed or removed: the stamp of the newest row of the
// rules feed, which every such write adds to. One Query for one row, never the whole feed. The time is zero when the
// feed has no row (a fresh install, or no change in the 90 days a row lives).
//
// judge is false when the newest row's sort key holds no readable time, which only a row edited by hand does. The
// outlier check is then skipped for this run. Reading it as "no change" instead would compare every Mac in the
// window, and after a real change that most Macs have not synced since, that lists the Macs that are up to date;
// listing nothing for a run cannot mislead, and the log says why.
func (r *run) lastRuleChange(ctx context.Context) (at time.Time, judge bool, err error) {
	out, err := r.opts.Client.Query(ctx, &dynamodb.QueryInput{
		TableName:                 aws.String(r.opts.StoreTable),
		KeyConditionExpression:    aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": str(feedrules.Partition)},
		ProjectionExpression:      aws.String("SK"),
		ScanIndexForward:          aws.Bool(false),
		Limit:                     aws.Int32(1),
	})
	if err != nil {
		return time.Time{}, false, fmt.Errorf("fleet: newest %s row: %w", feedrules.Partition, err)
	}
	if len(out.Items) == 0 {
		return time.Time{}, true, nil
	}
	sk, _ := out.Items[0]["SK"].(*types.AttributeValueMemberS)
	if sk != nil {
		if at, err := feedrules.TimeOfSortKey(sk.Value); err == nil {
			return at, true, nil
		}
	}
	log.Printf("fleet: rules hash outliers not checked in this run: the newest %s row's sort key holds no time", feedrules.Partition)
	return time.Time{}, false, nil
}

// machineIDs lists every Mac that has sensor data, Macs without a serial number or hostname included. It stops as
// soon as there are more than MaxMacs.
func (r *run) machineIDs(ctx context.Context) ([]string, error) {
	var ids []string
	err := r.query(ctx, &dynamodb.QueryInput{
		TableName:                 aws.String(r.opts.StoreTable),
		IndexName:                 aws.String(machineIndex),
		KeyConditionExpression:    aws.String("DataType = :dt"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":dt": str(string(sensordata.GetDataType()))},
		ProjectionExpression:      aws.String("MachineID"),
	}, func(item map[string]types.AttributeValue) error {
		id, ok := item["MachineID"].(*types.AttributeValueMemberS)
		if !ok || id.Value == "" {
			return nil
		}
		if len(ids) >= r.opts.MaxMacs {
			return fmt.Errorf("fleet: more than %d Macs; nothing was written (raise FLEET_MAX_MACS only with the Lambda's timeout and memory in mind)", r.opts.MaxMacs)
		}
		ids = append(ids, id.Value)
		return nil
	})
	return ids, err
}

// query pages a Query to its end.
func (r *run) query(ctx context.Context, in *dynamodb.QueryInput, each func(map[string]types.AttributeValue) error) error {
	for {
		out, err := r.opts.Client.Query(ctx, in)
		if err != nil {
			return err
		}
		for _, item := range out.Items {
			if err := each(item); err != nil {
				return err
			}
		}
		if len(out.LastEvaluatedKey) == 0 {
			return nil
		}
		in.ExclusiveStartKey = out.LastEvaluatedKey
	}
}

// machine reads one Mac, writes its rows and adds it to the totals.
func (r *run) machine(ctx context.Context, id string) error {
	m, err := r.read(ctx, id)
	if err != nil {
		return err
	}
	if m == nil {
		return nil
	}
	rep := classify(m, r.settings)

	// A Mac Rudolph already intends to lock needs no readiness figure, so its markers are not read at all: as a fleet
	// moves to Lockdown the job's reads, and its cost, fall with it.
	var unknown unknownCounts
	if !rep.locked {
		if unknown, err = r.unknown(ctx, id); err != nil {
			return err
		}
	}
	bucket := rep.bucket(unknown)

	var requests []types.WriteRequest
	for _, row := range r.healthRows(m, rep) {
		requests = append(requests, types.WriteRequest{PutRequest: &types.PutRequest{Item: row}})
	}
	ready := r.readinessRow(m, rep, unknown, bucket)
	scopes := append([]string{scopeAll}, rep.groups...)
	for _, scope := range scopes {
		row := make(map[string]types.AttributeValue, len(ready)+1)
		for k, v := range ready {
			row[k] = v
		}
		pk := snapshotPKPrefix + r.snapshot + "#Ready#All"
		if scope != scopeAll {
			pk = snapshotPKPrefix + r.snapshot + "#Ready#Group#" + scope
		}
		row["PK"] = str(pk)
		requests = append(requests, types.WriteRequest{PutRequest: &types.PutRequest{Item: row}})
	}
	if err := r.write(ctx, requests); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	t := &r.totals
	t.macs++
	listed := rep.categories()
	for _, category := range listed {
		t.health[category]++
	}
	if len(listed) > 0 {
		t.attention++
	}
	if rep.versionUnknown {
		t.versionUnknown++
	}
	if unknown.capped {
		t.capped++
	}
	for _, scope := range scopes {
		c := t.readiness[scope]
		if c == nil {
			// groups.Effective only returns registered groups and Ungrouped, which all have a scope
			continue
		}
		c.Macs++
		switch bucket {
		case bucketReady:
			c.Ready++
		case bucketFew:
			c.Few++
		case bucketMany:
			c.Many++
		case bucketUnmeasured:
			c.Unmeasured++
		case bucketLocked:
			c.Locked++
		}
	}
	if s, ok := rep.hashSample(m, r.settings.now); ok {
		s.attention = len(listed) > 0
		s.hash = r.intern(s.hash)
		digest := r.intern(m.state.MachineRulesTracked)
		r.samples[digest] = append(r.samples[digest], s)
	}
	return nil
}

func (r *run) intern(s string) string {
	if kept, ok := r.interned[s]; ok {
		return kept
	}
	r.interned[s] = s
	return s
}

// read fetches a Mac's config, sensor data, groups and sync state with one Query of its partition. It returns nil for
// a Mac whose sensor data is gone (expired since the index was read) or cannot be decoded.
func (r *run) read(ctx context.Context, id string) (*machine, error) {
	m := &machine{id: id}
	configKey := machineconfiguration.MachineConfigurationKey(id)
	groupsKey := groups.MachineGroupsKey(id)
	stateKey := syncstate.Key(id)
	_, sensorSK := sensordata.MachineIDSensorDataPKSK(id)
	err := r.query(ctx, &dynamodb.QueryInput{
		TableName: aws.String(r.opts.StoreTable),
		// The rows the job needs are the first and the last of the partition's range, so one Query reads them all.
		// The reported groups item between them comes along; it is small unless a Mac reports hundreds of groups.
		KeyConditionExpression: aws.String("PK = :pk AND SK BETWEEN :first AND :last"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": str(configKey.PartitionKey), ":first": str(configKey.SortKey), ":last": str(stateKey.SortKey),
		},
	}, func(item map[string]types.AttributeValue) error {
		sk, _ := item["SK"].(*types.AttributeValueMemberS)
		if sk == nil {
			return nil
		}
		var err error
		switch sk.Value {
		case configKey.SortKey:
			m.configItem = item
		case sensorSK:
			m.sensor = &sensordata.SensorData{}
			err = attributevalue.UnmarshalMap(item, m.sensor)
		case groupsKey.SortKey:
			m.groups, err = groups.MachineGroupsFromItem(item)
		case stateKey.SortKey:
			m.state = &syncstate.SyncStateRow{}
			err = attributevalue.UnmarshalMap(item, m.state)
		}
		if err != nil {
			// One row edited by hand must not stop the whole fleet's lists; the Mac is left out and the log says why.
			log.Printf("fleet: machine %s left out: its %s row cannot be decoded: %v", id, sk.Value, err)
			m.undecodable = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if m.sensor == nil || m.undecodable {
		return nil, nil
	}
	if m.groups == nil {
		m.groups = &groups.MachineGroups{}
	}
	return m, nil
}

// unknownCounts are a Mac's unknown binaries in the two windows; pending leaves out those a global rule now covers.
type unknownCounts struct {
	unknown30, pending30, unknown7, pending7 int
	capped                                   bool
}

// unknown reads a Mac's unknown markers with one Query and counts them. "Ran in the last N days" is LastDay on or
// after today minus N-1; expired markers DynamoDB has not deleted yet fall outside by their date.
func (r *run) unknown(ctx context.Context, id string) (unknownCounts, error) {
	out, err := r.opts.Client.Query(ctx, &dynamodb.QueryInput{
		TableName:                 aws.String(r.opts.Table),
		KeyConditionExpression:    aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": str(unknownMarkerPrefix + id)},
		ProjectionExpression:      aws.String("SK, LastDay, SigningID, TeamID"),
		Limit:                     aws.Int32(markerLimit),
	})
	if err != nil {
		return unknownCounts{}, fmt.Errorf("unknown markers: %w", err)
	}
	var c unknownCounts
	// A page cut short of the limit (by its size) also means there is more than was counted.
	c.capped = len(out.Items) >= markerLimit || (len(out.LastEvaluatedKey) > 0 && len(out.Items) > 0)
	from30 := r.settings.today.AddDate(0, 0, -29).Format(dayLayout)
	from7 := r.settings.today.AddDate(0, 0, -6).Format(dayLayout)
	for _, item := range out.Items {
		day := attrString(item, "LastDay")
		if day < from30 {
			continue
		}
		covered, err := r.covered(ctx, attrString(item, "SK"), attrString(item, "SigningID"), attrString(item, "TeamID"))
		if err != nil {
			return unknownCounts{}, err
		}
		c.unknown30++
		if !covered {
			c.pending30++
		}
		if day >= from7 {
			c.unknown7++
			if !covered {
				c.pending7++
			}
		}
	}
	return c, nil
}

// covered reports whether a global rule now decides the binary: by SHA-256, signing ID or team ID, the keys the
// ranker marks its unknown lists with. Certificate and CDHash rules cannot be matched (a marker holds neither), and
// group and machine rules are not counted, so pending can only overstate.
func (r *run) covered(ctx context.Context, sha, signingID, teamID string) (bool, error) {
	r.rulesOnce.Do(func() {
		keys := map[string]struct{}{}
		r.rulesErr = r.query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String(r.opts.StoreTable),
			KeyConditionExpression:    aws.String("PK = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":pk": str(globalRulesPK)},
			ProjectionExpression:      aws.String("SK"),
		}, func(item map[string]types.AttributeValue) error {
			keys[attrString(item, "SK")] = struct{}{}
			return nil
		})
		r.rules = keys
	})
	if r.rulesErr != nil {
		return false, fmt.Errorf("global rules: %w", r.rulesErr)
	}
	// Markers hold only values a day counter may carry, but one written by other tooling could hold anything.
	for _, sk := range rules.CoveringSortKeys(sha, counterfields.SigningID(signingID), counterfields.TeamID(teamID)) {
		if _, ok := r.rules[sk]; ok {
			return true, nil
		}
	}
	return false, nil
}

// write sends rows in batches, paced.
func (r *run) write(ctx context.Context, requests []types.WriteRequest) error {
	for len(requests) > 0 {
		n := min(snapshot.MaxBatch, len(requests))
		if err := r.pacer.wait(ctx, n); err != nil {
			return err
		}
		if err := snapshot.WriteBatch(ctx, r.opts.Client, r.opts.Table, requests[:n]); err != nil {
			return fmt.Errorf("fleet: %w", err)
		}
		requests = requests[n:]
	}
	return nil
}

// pacer spaces writes so that no more than one row per perRow is sent on average.
type pacer struct {
	mu     sync.Mutex
	next   time.Time
	perRow time.Duration
}

func (p *pacer) wait(ctx context.Context, rows int) error {
	p.mu.Lock()
	now := time.Now()
	if p.next.Before(now) {
		p.next = now
	}
	delay := p.next.Sub(now)
	p.next = p.next.Add(time.Duration(rows) * p.perRow)
	p.mu.Unlock()
	if delay <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

func str(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

func num(v int) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.Itoa(v)}
}

func attrString(item map[string]types.AttributeValue, name string) string {
	v, _ := item[name].(*types.AttributeValueMemberS)
	if v == nil {
		return ""
	}
	return v.Value
}
