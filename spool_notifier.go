package ghostline

import (
	"sync"
	"time"
)

type spoolNotifier interface {
	Events() <-chan struct{}
	Close()
}

type pollingSpoolNotifier struct {
	events    chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	wait      sync.WaitGroup
}

func newPollingSpoolNotifier() spoolNotifier {
	notifier := &pollingSpoolNotifier{
		events: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	notifier.wait.Add(1)
	go notifier.run()
	return notifier
}

func (n *pollingSpoolNotifier) Events() <-chan struct{} {
	return n.events
}

func (n *pollingSpoolNotifier) Close() {
	n.closeOnce.Do(func() {
		close(n.done)
		n.wait.Wait()
	})
}

func (n *pollingSpoolNotifier) run() {
	defer n.wait.Done()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-n.done:
			return
		case <-ticker.C:
			select {
			case n.events <- struct{}{}:
			default:
			}
		}
	}
}
