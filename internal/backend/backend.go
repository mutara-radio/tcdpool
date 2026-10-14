// Package backend defines the interface between the tcdpool process
// manager and one kind of transcoder process. The nostar tcd is one
// backend (package nostar). Another tcd flavour can be added as another
// package without changes to the pool or the client protocol.
package backend

import (
	"context"
	"errors"
	"time"

	"github.com/dbehnke/tcdpool/internal/codec"
)

// Frame is one 20 ms voice frame, in or out of a backend. Only the
// fields named in Have hold data.
type Frame struct {
	StreamID uint16
	Seq      uint32
	Last     bool
	In       codec.ID  // the codec the client sent
	Want     codec.Set // to a backend: the outputs to make (0 = all)
	Have     codec.Set // fields that hold data
	PCM      [codec.FrameSamples]int16
	DStar    [9]byte
	DMR      [9]byte
	IMBE     [11]byte
	C2       [8]byte
}

// Payload returns the bytes of codec id in f (nil for PCM).
func (f *Frame) Payload(id codec.ID) []byte {
	switch id {
	case codec.DStar:
		return f.DStar[:]
	case codec.DMR:
		return f.DMR[:]
	case codec.IMBE:
		return f.IMBE[:]
	case codec.C2:
		return f.C2[:]
	}
	return nil
}

// Capabilities says what a backend can do.
type Capabilities struct {
	Inputs  codec.Set // codecs a client may send
	Outputs codec.Set // codecs a client may ask for
}

// Backend starts transcoder instances.
type Backend interface {
	// Name is a short name for logs and for the client ("nostar-tcd").
	Name() string
	Capabilities() Capabilities
	// Start starts one instance and returns when it is ready to take
	// frames, or with an error. id names the instance in logs.
	Start(ctx context.Context, id string) (Instance, error)
}

// Instance is one running transcoder process with its link.
type Instance interface {
	// Send passes one frame to the transcoder. It must not block for
	// long; it returns an error when the link is down.
	Send(f *Frame) error
	// Recv returns the next reply. It returns an error when the
	// instance has stopped: ErrDesync (wrapped) when the link went out of
	// step, ErrExited (wrapped) when the process ended, or another error.
	Recv() (*Frame, error)
	// Stop ends the process and closes the link. It is safe to call
	// more than once.
	Stop()
	// PID returns the process ID, or 0.
	PID() int
	// LinkStats returns counters of the link (bad frames and so on).
	LinkStats() LinkStats
}

// LinkStats are counters kept by an instance link.
type LinkStats struct {
	Pings      uint64 // keep-alive packets from the transcoder
	BadFrames  uint64 // v2 frames that failed a check and were skipped
	SkipBytes  uint64 // bytes skipped while looking for the next frame
	LastPacket time.Time
}

var (
	// ErrDesync means the link carried data that cannot be a valid
	// reply; the instance must be restarted.
	ErrDesync = errors.New("link out of step")
	// ErrExited means the process ended.
	ErrExited = errors.New("process exited")
	// ErrStalled means the transcoder sent nothing, not even a
	// keep-alive, for too long.
	ErrStalled = errors.New("link stalled")
)
