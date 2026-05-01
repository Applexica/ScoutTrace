package wire

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

// ForwardSignals installs a handler that calls onSignal with each forwarded
// signal (currently SIGTERM and SIGINT). Returns a stop function that
// unregisters the handler.
//
// The grace duration is exposed so callers can implement the §6.4 shutdown
// sequence: stop accepting frames, drain capture, then exit.
func ForwardSignals(grace time.Duration, onSignal func(os.Signal)) func() {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	stopCh := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-ch:
				if onSignal != nil {
					onSignal(s)
				}
			case <-stopCh:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(stopCh)
		_ = grace // reserved for future grace handling
	}
}
