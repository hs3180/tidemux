package gateway

import "sync/atomic"

// providerKeyPool chooses a credential for one provider profile. The selected
// key is copied into a request-local adapter client, so a streaming request
// keeps the same key for its entire lifetime.
type providerKeyPool struct {
	keys []string
	next atomic.Uint64
}

func newProviderKeyPool(keys []string) *providerKeyPool {
	if len(keys) < 2 {
		return nil
	}
	return &providerKeyPool{keys: append([]string(nil), keys...)}
}

func (p *providerKeyPool) Next() string {
	index := (p.next.Add(1) - 1) % uint64(len(p.keys))
	return p.keys[index]
}
