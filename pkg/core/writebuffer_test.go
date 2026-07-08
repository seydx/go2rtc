package core

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A WriteTo still blocked on a keyframe must unblock on Close, or the consumer leaks.
func TestWriteBufferCloseUnblocksWriteTo(t *testing.T) {
	wb := NewWriteBuffer(nil)

	returned := make(chan struct{})
	go func() {
		_, _ = wb.WriteTo(&OnceBuffer{}) // no keyframe is ever written
		close(returned)
	}()

	// WriteTo must still be blocked: no keyframe, no Close yet.
	select {
	case <-returned:
		t.Fatal("WriteTo returned before any keyframe or Close")
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, wb.Close())

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("WriteTo did not unblock after Close - the consumer would leak")
	}
}

// Close before WriteTo registers its wait must not block WriteTo forever.
func TestWriteBufferCloseBeforeWriteTo(t *testing.T) {
	wb := NewWriteBuffer(nil)
	require.NoError(t, wb.Close())

	returned := make(chan struct{})
	go func() {
		_, _ = wb.WriteTo(&OnceBuffer{})
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("WriteTo blocked after Close-before-WriteTo - the race window leaks the consumer")
	}
}

// recyclableWriter emulates a net/http response writer: after the handler
// returns (i.e. WriteTo unblocks), net/http puts the response bufio.Writer
// back into a pool. Any Write after that moment reproduces the production
// panics (nil deref / slice bounds out of range inside bufio).
type recyclableWriter struct {
	first    sync.Once
	ready    chan struct{}
	recycled atomic.Bool
	violated atomic.Bool
}

func (w *recyclableWriter) Write(p []byte) (int, error) {
	w.first.Do(func() { close(w.ready) })
	if w.recycled.Load() {
		w.violated.Store(true)
	}
	return len(p), nil
}

func TestWriteBufferCloseBlocksLateWrites(t *testing.T) {
	wb := NewWriteBuffer(nil)
	rw := &recyclableWriter{ready: make(chan struct{})}

	// init segment is buffered before WriteTo, Reset will copy it into rw
	_, _ = wb.Write([]byte("init"))

	handlerDone := make(chan struct{})
	go func() {
		_, _ = wb.WriteTo(rw) // handlerMP4: blocks until Close
		rw.recycled.Store(true)
		close(handlerDone)
	}()
	<-rw.ready // WriteTo has been called, Writer is switched to rw

	// track sender goroutines keep pushing frames
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100000; j++ {
				if _, err := wb.Write([]byte("frame")); err != nil {
					return
				}
			}
		}()
	}

	_ = wb.Close() // client disconnected -> cons.Stop() -> Transport.Close()
	<-handlerDone
	wg.Wait()

	if rw.violated.Load() {
		t.Fatal("Write after handler return: production panic scenario reproduced")
	}
	if _, err := wb.Write([]byte("late")); err == nil {
		t.Fatal("Write after Close must return error")
	}
}
