package snmp_lookup

import (
	"sync"
	"time"

	"github.com/alitto/pond"
	"github.com/hashicorp/golang-lru/v2/expirable"

	"github.com/influxdata/telegraf/config"
)

type store struct {
	cache                *expirable.LRU[string, *tagMap]
	pool                 *pond.WorkerPool
	minUpdateInterval    time.Duration
	inflight             sync.Map
	deferredUpdates      map[string]time.Time
	deferredUpdatesTimer *time.Timer
	notify               func(string, *tagMap)
	update               func(string) *tagMap
	stopped              bool // Add flag to track if store is being destroyed

	sync.Mutex
}

func newStore(size int, ttl config.Duration, workers int, minUpdateInterval config.Duration) *store {
	return &store{
		cache:             expirable.NewLRU[string, *tagMap](size, nil, time.Duration(ttl)),
		pool:              pond.New(workers, 0, pond.MinWorkers(workers/2+1)),
		deferredUpdates:   make(map[string]time.Time),
		minUpdateInterval: time.Duration(minUpdateInterval),
		stopped:           false,
	}
}

func (s *store) addBacklog(agent string, earliest time.Time) {
	s.Lock()
	defer s.Unlock()

	// Don't add to the backlog if store is being destroyed
	if s.stopped {
		return
	}

	t, found := s.deferredUpdates[agent]
	if !found || t.After(earliest) {
		s.deferredUpdates[agent] = earliest
		s.refreshTimer()
	}
}

func (s *store) removeBacklog(agent string) {
	s.Lock()
	defer s.Unlock()

	// Still allow removal even if stopped to prevent deadlocks
	delete(s.deferredUpdates, agent)
	if !s.stopped {
		s.refreshTimer()
	}
}

func (s *store) refreshTimer() {
	if s.deferredUpdatesTimer != nil {
		s.deferredUpdatesTimer.Stop()
	}
	if len(s.deferredUpdates) == 0 || s.stopped {
		return
	}
	var agent string
	var earliest time.Time
	for k, t := range s.deferredUpdates {
		if agent == "" || t.Before(earliest) {
			agent = k
			earliest = t
		}
	}
	s.deferredUpdatesTimer = time.AfterFunc(time.Until(earliest), func() { s.enqueue(agent) })
}

func (s *store) enqueue(agent string) {
	if _, inflight := s.inflight.LoadOrStore(agent, true); inflight || s.pool.Stopped() {
		return
	}
	s.pool.Submit(func() {
		entry := s.update(agent)
		s.cache.Add(agent, entry)
		s.removeBacklog(agent)
		s.notify(agent, entry)
		s.inflight.Delete(agent)
	})
}

func (s *store) lookup(agent, index string) {
	entry, cached := s.cache.Get(agent)
	if !cached {
		// There is no cache at all, so we need to enqueue an update.
		s.enqueue(agent)
		return
	}

	// In case the index does not exist, we need to update the agent as this
	// new index might have been added in the meantime (e.g. after hot-plugging
	// hardware). In any way, we release the metric unresolved to not block
	// ordered operations for long time.
	if _, found := entry.rows[index]; !found {
		// Only update the agent if the user wants to
		if s.minUpdateInterval > 0 {
			if time.Since(entry.created) > s.minUpdateInterval {
				// The minimum time between updates has passed so we are good to
				// directly update the cache.
				s.enqueue(agent)
				return
			}
			// The minimum time between updates has not yet passed so we
			// need to defer the agent update to later.
			s.addBacklog(agent, entry.created.Add(s.minUpdateInterval))
		}
	}

	s.notify(agent, entry)
}

func (s *store) destroy() {
	// First, acquire lock and stop accepting new work
	s.Lock()
	s.stopped = true

	// Clear deferred updates and stop timer
	s.deferredUpdates = make(map[string]time.Time)
	if s.deferredUpdatesTimer != nil {
		s.deferredUpdatesTimer.Stop()
		s.deferredUpdatesTimer = nil
	}
	s.Unlock()

	// Now wait for worker pool to finish WITHOUT holding the lock
	// This prevents the deadlock where workers need the lock in removeBacklog()
	s.pool.StopAndWait()
}

func (s *store) purge() {
	s.Lock()
	defer s.Unlock()
	s.cache.Purge()
}
