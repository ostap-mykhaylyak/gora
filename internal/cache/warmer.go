package cache

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ostap-mykhaylyak/gora/internal/pool"
)

// warmDebounce coalesces bursts of invalidations into a single refetch:
// saving a settings page issues dozens of option UPDATEs, and refetching
// the snapshot after each one would cost more than the cache saves.
const warmDebounce = 200 * time.Millisecond

// Warmer repopulates invalidated alloptions snapshots in the background, so
// the visitor arriving right after an option write finds the cache warm
// instead of paying for the heaviest query of the pageload.
type Warmer struct {
	cache *Cache
	pool  *pool.Pool
	log   *slog.Logger

	mu      sync.Mutex
	pending map[string]string // db -> the alloptions query to replay
	kick    chan struct{}
}

// NewWarmer builds the warmer; wire it with cache.SetRefetch(w.Trigger) and
// start it with Run.
func NewWarmer(c *Cache, p *pool.Pool, log *slog.Logger) *Warmer {
	return &Warmer{
		cache:   c,
		pool:    p,
		log:     log,
		pending: make(map[string]string),
		kick:    make(chan struct{}, 1),
	}
}

// Trigger schedules a refetch. It is called from the invalidation path, so
// it never blocks and never touches the database itself.
func (w *Warmer) Trigger(db, query string) {
	w.mu.Lock()
	w.pending[db] = query
	w.mu.Unlock()
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// Run processes refetches until ctx is cancelled.
func (w *Warmer) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.kick:
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(warmDebounce):
		}

		w.mu.Lock()
		batch := w.pending
		w.pending = make(map[string]string)
		w.mu.Unlock()

		for db, query := range batch {
			w.refetch(ctx, db, query)
		}
	}
}

func (w *Warmer) refetch(ctx context.Context, db, query string) {
	conn, err := w.pool.Acquire(ctx)
	if err != nil {
		w.log.Debug("cache warm-up skipped, no backend connection", "database", db, "error", err)
		return
	}

	if db != "" && conn.BoundDB != db {
		if err := conn.UseDB(db); err != nil {
			w.log.Debug("cache warm-up failed", "database", db, "error", err)
			w.pool.Release(conn)
			return
		}
		conn.BoundDB = db
	}
	r, err := conn.Execute(query)
	if err != nil {
		w.log.Debug("cache warm-up failed", "database", db, "error", err)
		w.pool.Release(conn)
		return
	}
	w.pool.ReleaseClean(conn)

	w.cache.Warm(db, query, r)
	w.log.Debug("cache warmed", "database", db)
}
