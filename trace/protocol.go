package trace

import "sync"

// UsageScanner extracts token usage from one traced response body. Write
// receives the response bytes as they stream through the tracer; Result
// reports the usage seen so far, nil until something was extracted. The
// returned usage follows the upstream's own claim: input including cache
// only for dialects that report it that way (see Tracer.CacheInInput).
type UsageScanner interface {
	Write(p []byte)
	Result() *Usage
}

// UsageProtocol is a pluggable usage-extraction dialect. Modules that
// translate private or otherwise unparsable wire formats implement one and
// register it in init via RegisterUsageProtocol; a tracer then selects it
// by name with the `protocol` option. The implementing package must be
// linked in (blank import from the binary's main package).
type UsageProtocol interface {
	// Name is the value the `protocol` option selects the protocol by.
	Name() string
	// NewScanner returns a fresh scanner for one response.
	NewScanner() UsageScanner
}

var (
	protocolsMu sync.RWMutex
	protocols   = map[string]UsageProtocol{}
)

// RegisterUsageProtocol makes a protocol selectable by name. Registering
// the same name twice replaces the earlier registration.
func RegisterUsageProtocol(p UsageProtocol) {
	protocolsMu.Lock()
	defer protocolsMu.Unlock()
	protocols[p.Name()] = p
}

// usageProtocol resolves a configured protocol name; nil when unset.
func usageProtocol(name string) UsageProtocol {
	if name == "" {
		return nil
	}
	protocolsMu.RLock()
	defer protocolsMu.RUnlock()
	return protocols[name]
}

// newUsageScanner picks the scanner for one response: the configured
// protocol's, or the built-in multi-dialect one.
func (t *Tracer) newUsageScanner() UsageScanner {
	if p := usageProtocol(t.Protocol); p != nil {
		return p.NewScanner()
	}
	return &usageScanner{}
}
