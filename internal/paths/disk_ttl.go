package paths

import (
	"context"
	"os"
	"sync"
	"time"
)

// DiskTTL caches one fetched value in Path for TTL (±Jitter), one process fetching while the others wait.
type DiskTTL[T any] struct {
	Name   string
	Path   string
	TTL    time.Duration
	Jitter time.Duration
	Decode func([]byte) (T, error)
	Encode func(T) ([]byte, error)

	once  sync.Once
	stats *cacheStats
}

// Get returns the fresh file's value, else fetches under Path's lock; fetch errors are never written.
func (d *DiskTTL[T]) Get(ctx context.Context, fetch func(context.Context) (T, error)) (T, error) {
	d.once.Do(func() { d.stats = registerStats(d.Name) })
	if v, ok, _ := d.read(); ok {
		d.stats.hit()
		return v, nil
	}
	var out T
	var ferr error
	fetched := false
	lerr := WithFileLock(d.Path, func() error {
		v, ok, existed := d.read()
		if ok {
			out = v
			d.stats.hit()
			return nil
		}
		d.stats.miss()
		if existed {
			d.stats.refresh()
		}
		fetched = true
		if out, ferr = fetch(ctx); ferr == nil {
			d.write(out)
		}
		return nil
	})
	if lerr != nil && !fetched { // lock failure never fails closed
		d.stats.miss()
		return fetch(ctx)
	}
	return out, ferr
}

// read decodes the file when it is fresh; existed reports a file that was there but stale or unreadable.
func (d *DiskTTL[T]) read() (v T, fresh, existed bool) {
	fi, err := os.Stat(d.Path)
	if err != nil {
		return v, false, false
	}
	if Stale(fi.ModTime(), d.ttlFor(fi.ModTime()), racyWindow, stampNow()) {
		return v, false, true
	}
	data, err := os.ReadFile(d.Path)
	if err != nil {
		return v, false, true
	}
	v, err = d.Decode(data)
	return v, err == nil, true
}

// ttlFor is TTL shifted within ±Jitter by a hash of mtime, so concurrent readers agree on one expiry.
func (d *DiskTTL[T]) ttlFor(mtime time.Time) time.Duration {
	if d.Jitter <= 0 {
		return d.TTL
	}
	h := uint64(mtime.UnixNano()) * 0x9E3779B97F4A7C15
	return d.TTL + time.Duration((h>>11)%uint64(2*d.Jitter+1)) - d.Jitter
}

func (d *DiskTTL[T]) write(v T) {
	if data, err := d.Encode(v); err == nil {
		_ = AtomicWrite(d.Path, data, 0o600)
	}
}
