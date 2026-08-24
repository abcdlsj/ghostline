//go:build darwin

package ghostline

import (
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

type kqueueSpoolNotifier struct {
	queue     int
	events    chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	wait      sync.WaitGroup
}

func newSpoolNotifier(_ string, file *os.File) spoolNotifier {
	queue, err := unix.Kqueue()
	if err != nil {
		return newPollingSpoolNotifier()
	}
	var vnode unix.Kevent_t
	unix.SetKevent(&vnode, int(file.Fd()), unix.EVFILT_VNODE, unix.EV_ADD|unix.EV_CLEAR)
	vnode.Fflags = unix.NOTE_WRITE | unix.NOTE_EXTEND | unix.NOTE_ATTRIB | unix.NOTE_RENAME | unix.NOTE_DELETE
	var wake unix.Kevent_t
	unix.SetKevent(&wake, 1, unix.EVFILT_USER, unix.EV_ADD|unix.EV_CLEAR)
	if _, err := unix.Kevent(queue, []unix.Kevent_t{vnode, wake}, nil, nil); err != nil {
		_ = unix.Close(queue)
		return newPollingSpoolNotifier()
	}
	notifier := &kqueueSpoolNotifier{
		queue:  queue,
		events: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	notifier.wait.Add(1)
	go notifier.run()
	return notifier
}

func (n *kqueueSpoolNotifier) Events() <-chan struct{} {
	return n.events
}

func (n *kqueueSpoolNotifier) Close() {
	n.closeOnce.Do(func() {
		close(n.done)
		var wake unix.Kevent_t
		unix.SetKevent(&wake, 1, unix.EVFILT_USER, 0)
		wake.Fflags = unix.NOTE_TRIGGER
		if _, err := unix.Kevent(n.queue, []unix.Kevent_t{wake}, nil, nil); err != nil {
			_ = unix.Close(n.queue)
		}
		n.wait.Wait()
		_ = unix.Close(n.queue)
	})
}

func (n *kqueueSpoolNotifier) run() {
	defer n.wait.Done()
	events := make([]unix.Kevent_t, 1)
	for {
		if _, err := unix.Kevent(n.queue, nil, events, nil); err != nil {
			return
		}
		select {
		case <-n.done:
			return
		default:
		}
		select {
		case n.events <- struct{}{}:
		default:
		}
	}
}
