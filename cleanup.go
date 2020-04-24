package pgstore

import (
	"time"
)

var defaultInterval = time.Minute * 5

// Cleanup runs a background goroutine every interval that deletes expired
// sessions from the database.
//
// The design is based on https://github.com/yosssi/boltstore
func (p *PGStore) Cleanup(interval time.Duration) (chan<- struct{}, <-chan struct{}) {
	if interval <= 0 {
		interval = defaultInterval
	}

	quit, done := make(chan struct{}), make(chan struct{})
	go p.cleanup(interval, quit, done)
	return quit, done
}

// StopCleanup stops the background cleanup from running.
func StopCleanup(quit chan<- struct{}, done <-chan struct{}) {
	quit <- struct{}{}
	<-done
}

// cleanup deletes expired sessions at set intervals.
func (p *PGStore) cleanup(interval time.Duration, quit <-chan struct{}, done chan<- struct{}) {
	ticker := time.NewTicker(interval)

	defer func() {
		ticker.Stop()
	}()

	for {
		select {
		case <-quit:
			// Handle the quit signal.
			done <- struct{}{}
			return
		case <-ticker.C:
			// Delete expired sessions on each tick.
			err := p.DeleteExpired()
			if err != nil {
				p.logger.Errorf("pgstore: unable to delete expired sessions: %v", err)
			}
		}
	}
}

// DeleteExpired deletes expired sessions from the database.
func (p *PGStore) DeleteExpired() error {
	_, err := p.db.Exec("DELETE FROM http_sessions WHERE expires_on < now()")
	return err
}
