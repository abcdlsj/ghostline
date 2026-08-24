//go:build linux

package ghostline

import (
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

type inotifySpoolNotifier struct {
	descriptor int
	wake       int
	events     chan struct{}
	done       chan struct{}
	closeOnce  sync.Once
	wait       sync.WaitGroup
}

func newSpoolNotifier(path string, _ *os.File) spoolNotifier {
	descriptor, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return newPollingSpoolNotifier()
	}
	mask := uint32(unix.IN_MODIFY | unix.IN_ATTRIB | unix.IN_CLOSE_WRITE | unix.IN_MOVE_SELF | unix.IN_DELETE_SELF)
	if _, err := unix.InotifyAddWatch(descriptor, path, mask); err != nil {
		_ = unix.Close(descriptor)
		return newPollingSpoolNotifier()
	}
	wake, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		_ = unix.Close(descriptor)
		return newPollingSpoolNotifier()
	}
	notifier := &inotifySpoolNotifier{
		descriptor: descriptor,
		wake:       wake,
		events:     make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	notifier.wait.Add(1)
	go notifier.run()
	return notifier
}

func (n *inotifySpoolNotifier) Events() <-chan struct{} {
	return n.events
}

func (n *inotifySpoolNotifier) Close() {
	n.closeOnce.Do(func() {
		close(n.done)
		_, _ = unix.Write(n.wake, []byte{1, 0, 0, 0, 0, 0, 0, 0})
		n.wait.Wait()
		_ = unix.Close(n.wake)
		_ = unix.Close(n.descriptor)
	})
}

func (n *inotifySpoolNotifier) run() {
	defer n.wait.Done()
	buffer := make([]byte, unix.SizeofInotifyEvent*16)
	poll := []unix.PollFd{
		{Fd: int32(n.descriptor), Events: unix.POLLIN},
		{Fd: int32(n.wake), Events: unix.POLLIN},
	}
	for {
		if _, err := unix.Poll(poll, -1); err != nil {
			if err == unix.EINTR {
				continue
			}
			return
		}
		if poll[1].Revents&unix.POLLIN != 0 {
			return
		}
		if poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return
		}
		if poll[0].Revents&unix.POLLIN == 0 {
			continue
		}
		if _, err := unix.Read(n.descriptor, buffer); err != nil && err != unix.EAGAIN {
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
