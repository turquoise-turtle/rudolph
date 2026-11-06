package groups

import (
	"sync"
	"time"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
)

// CacheDuration bounds how long a warm Lambda instance can use a registry read earlier, and so how long a change to
// groups or group rules takes to reach the Macs it syncs (plus their sync interval). Like the global config cache
// (machineconfiguration), most syncs then read nothing.
const CacheDuration = time.Minute

// Cache holds the registry and the group rules a server instance read last. Rules are kept by RulesVersion, so a
// group's rules are read again only when the registry says they changed. Each handler instance has its own cache.
type Cache struct {
	mu       sync.Mutex
	clock    clock.TimeProvider
	registry *Registry
	expires  time.Time
	rules    map[string]cachedRules
	maxAge   time.Duration
}

type cachedRules struct {
	version int
	rows    []RuleRow
}

func NewCache(timeProvider clock.TimeProvider) *Cache {
	return &Cache{clock: timeProvider, rules: map[string]cachedRules{}, maxAge: CacheDuration}
}

// Registry returns the registry, read (strongly consistently, so it is never older than the last one cached) at most
// CacheDuration ago
func (c *Cache) Registry(client dynamodb.GetItemAPI) (*Registry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	if c.registry != nil && now.Before(c.expires) {
		return c.registry, nil
	}
	registry, err := GetRegistry(client, true)
	if err != nil {
		return nil, err
	}
	c.registry, c.expires = registry, now.Add(c.maxAge)
	// Forget the rules of groups that are gone
	for group := range c.rules {
		if _, ok := registry.Groups[group]; !ok {
			delete(c.rules, group)
		}
	}
	return registry, nil
}

// Rules returns the rules of a registered group at the version the registry gives
func (c *Cache) Rules(client dynamodb.QueryAPI, registry *Registry, group string) ([]RuleRow, error) {
	entry, ok := registry.Groups[group]
	if !ok {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.rules[group]; ok && cached.version == entry.RulesVersion {
		return cached.rows, nil
	}
	rows, err := GetRules(client, group)
	if err != nil {
		return nil, err
	}
	c.rules[group] = cachedRules{version: entry.RulesVersion, rows: rows}
	return rows, nil
}
