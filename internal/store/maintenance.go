package store

// Background maintenance keeps open/recovery time bounded regardless of total
// data — Centauri's analogue of Oracle's incremental checkpoint + redo-log
// switch:
//
//   - Periodic checkpointing (plain log store): write the recovery checkpoint
//     on a cadence, not just on Close, so a crash replays only the log since the
//     last checkpoint instead of since the last clean shutdown.
//   - Auto-sealing (archive-backed store): when the appendable tail grows past a
//     threshold, roll it into a new compressed, Merkle-rooted segment (the
//     already-tested crash-safe Seal). The hot tail — and therefore the bytes
//     replayed on open — stays bounded no matter how much history accrues.
//
// Both reuse existing, tested primitives (writeCheckpoint, Seal); the loop just
// triggers them on a schedule. Started by Open when enabled, stopped by Close.

import "time"

// maintenanceEnabled reports whether either background task is configured.
func (s *Store) maintenanceEnabled() bool {
	if s.opts.CheckpointEvery > 0 && s.archiveDir == "" {
		return true
	}
	if s.opts.AutoSealBytes > 0 && s.archiveDir != "" && !s.opts.LazyPayloads {
		return true
	}
	return false
}

// startMaintenance launches the background loop if anything is configured.
func (s *Store) startMaintenance() {
	if !s.maintenanceEnabled() {
		return
	}
	s.maintStop = make(chan struct{})
	s.maintDone = make(chan struct{})
	go s.maintenanceLoop()
}

// stopMaintenance signals the loop and waits for it to drain (idempotent).
func (s *Store) stopMaintenance() {
	if s.maintStop == nil {
		return
	}
	s.maintStopOnce.Do(func() {
		close(s.maintStop)
		<-s.maintDone
	})
}

func (s *Store) maintenanceLoop() {
	const tick = time.Second
	t := time.NewTicker(tick)
	defer t.Stop()
	var sinceCkpt time.Duration
	for {
		select {
		case <-s.maintStop:
			close(s.maintDone)
			return
		case <-t.C:
			s.maybeAutoSeal()
			if s.opts.CheckpointEvery > 0 && s.archiveDir == "" {
				sinceCkpt += tick
				if sinceCkpt >= s.opts.CheckpointEvery {
					sinceCkpt = 0
					s.checkpointNow()
				}
			}
		}
	}
}

// maybeAutoSeal seals the tail when it has grown past AutoSealBytes. Seal takes
// s.mu and is crash-safe; failures (e.g. transient I/O) are retried next tick.
func (s *Store) maybeAutoSeal() {
	if s.archiveDir == "" || s.opts.AutoSealBytes <= 0 || s.opts.LazyPayloads {
		return
	}
	s.mu.RLock()
	sz := s.size
	s.mu.RUnlock()
	if sz >= s.opts.AutoSealBytes {
		_ = s.Seal() // best-effort; the manifest is the source of truth
	}
}

// checkpointNow writes the recovery checkpoint under the lock (best-effort: a
// stale/missing checkpoint only costs replay time, never correctness).
func (s *Store) checkpointNow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writable() == nil {
		_ = s.writeCheckpoint()
	}
}
