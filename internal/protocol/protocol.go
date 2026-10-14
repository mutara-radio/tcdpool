// Package protocol is the tcdpool client protocol: binary voice frames
// and JSON control messages on one WebSocket. docs/protocol.md is the
// full description; keep the two in step.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/dbehnke/tcdpool/internal/codec"
)

// Version is the protocol version, in the hello message and in byte 0
// of each binary frame.
const Version = 1

// Binary frame types (byte 1).
const (
	TypeRequest = 1 // client -> server
	TypeReply   = 2 // server -> client
)

// Header sizes.
const (
	RequestHeaderSize = 12
	ReplyHeaderSize   = 16
)

// Flag bits (byte 8).
const (
	FlagLast = 0x01 // last frame of the stream
	FlagLate = 0x02 // reply only: the reply was late
)

// Request is one voice frame from the client.
type Request struct {
	StreamID uint16
	Seq      uint32
	Last     bool
	In       codec.ID
	// Outputs are the codecs wanted in the reply. 0 means the session
	// default (from hello).
	Outputs codec.Set
	Payload []byte // In.Size() bytes
}

// ErrBadFrame is wrapped by decode errors.
var ErrBadFrame = errors.New("bad frame")

// AppendRequest appends the encoded request to b.
func AppendRequest(b []byte, r *Request) []byte {
	var flags byte
	if r.Last {
		flags |= FlagLast
	}
	b = append(b, Version, TypeRequest)
	b = binary.LittleEndian.AppendUint16(b, r.StreamID)
	b = binary.LittleEndian.AppendUint32(b, r.Seq)
	b = append(b, flags, byte(r.In), byte(r.Outputs), 0)
	return append(b, r.Payload...)
}

// DecodeRequest decodes a request. Payload points into b.
func DecodeRequest(b []byte) (Request, error) {
	var r Request
	if len(b) < RequestHeaderSize {
		return r, fmt.Errorf("%w: %d bytes, the header is %d", ErrBadFrame, len(b), RequestHeaderSize)
	}
	if b[0] != Version {
		return r, fmt.Errorf("%w: version %d", ErrBadFrame, b[0])
	}
	if b[1] != TypeRequest {
		return r, fmt.Errorf("%w: type %d is not a request", ErrBadFrame, b[1])
	}
	if b[8]&^FlagLast != 0 || b[11] != 0 {
		return r, fmt.Errorf("%w: unknown flag or reserved bits set", ErrBadFrame)
	}
	r.StreamID = binary.LittleEndian.Uint16(b[2:])
	r.Seq = binary.LittleEndian.Uint32(b[4:])
	r.Last = b[8]&FlagLast != 0
	r.In = codec.ID(b[9])
	r.Outputs = codec.Set(b[10])
	if !r.In.Valid() {
		return r, fmt.Errorf("%w: input codec %d", ErrBadFrame, b[9])
	}
	if !r.Outputs.Valid() {
		return r, fmt.Errorf("%w: output mask %#x has unknown bits", ErrBadFrame, b[10])
	}
	r.Payload = b[RequestHeaderSize:]
	if len(r.Payload) != r.In.Size() {
		return r, fmt.Errorf("%w: %v payload is %d bytes, want %d", ErrBadFrame, r.In, len(r.Payload), r.In.Size())
	}
	return r, nil
}

// Reply is one voice frame to the client.
type Reply struct {
	StreamID uint16
	Seq      uint32
	Last     bool
	Late     bool
	In       codec.ID
	Outputs  codec.Set // the payloads present, in codec ID order
	// LatencyMicros is the time in the transcoder: from the send to tcd
	// to the reply from tcd.
	LatencyMicros uint32
	// Payloads by codec; only those in Outputs are used.
	Payloads [codec.C2 + 1][]byte
}

// AppendReply appends the encoded reply to b.
func AppendReply(b []byte, r *Reply) []byte {
	var flags byte
	if r.Last {
		flags |= FlagLast
	}
	if r.Late {
		flags |= FlagLate
	}
	b = append(b, Version, TypeReply)
	b = binary.LittleEndian.AppendUint16(b, r.StreamID)
	b = binary.LittleEndian.AppendUint32(b, r.Seq)
	b = append(b, flags, byte(r.In), byte(r.Outputs), 0)
	b = binary.LittleEndian.AppendUint32(b, r.LatencyMicros)
	for _, id := range r.Outputs.IDs() {
		p := r.Payloads[id]
		if len(p) != id.Size() {
			p = make([]byte, id.Size()) // never short; zero if missing
		}
		b = append(b, p...)
	}
	return b
}

// DecodeReply decodes a reply. The payloads point into b.
func DecodeReply(b []byte) (Reply, error) {
	var r Reply
	if len(b) < ReplyHeaderSize {
		return r, fmt.Errorf("%w: %d bytes, the header is %d", ErrBadFrame, len(b), ReplyHeaderSize)
	}
	if b[0] != Version || b[1] != TypeReply {
		return r, fmt.Errorf("%w: version %d type %d", ErrBadFrame, b[0], b[1])
	}
	r.StreamID = binary.LittleEndian.Uint16(b[2:])
	r.Seq = binary.LittleEndian.Uint32(b[4:])
	r.Last = b[8]&FlagLast != 0
	r.Late = b[8]&FlagLate != 0
	r.In = codec.ID(b[9])
	r.Outputs = codec.Set(b[10])
	r.LatencyMicros = binary.LittleEndian.Uint32(b[12:])
	if !r.Outputs.Valid() {
		return r, fmt.Errorf("%w: output mask %#x", ErrBadFrame, b[10])
	}
	p := b[ReplyHeaderSize:]
	if len(p) != r.Outputs.PayloadSize() {
		return r, fmt.Errorf("%w: payload %d bytes, want %d", ErrBadFrame, len(p), r.Outputs.PayloadSize())
	}
	for _, id := range r.Outputs.IDs() {
		r.Payloads[id] = p[:id.Size()]
		p = p[id.Size():]
	}
	return r, nil
}

// Control message types (the "type" field of a JSON text message).
const (
	MsgHello      = "hello"
	MsgReady      = "ready"
	MsgRestarting = "restarting"
	MsgError      = "error"
	MsgStats      = "stats"
	MsgBye        = "bye"
)

// Hello is the first message from the client.
type Hello struct {
	Type    string `json:"type"`
	Version int    `json:"version"`
	// Session names the process with the key: a client that comes back
	// with the same key and session gets the same tcd process (if it
	// comes back before the idle timeout).
	Session string `json:"session"`
	// Outputs are the default outputs for a request with output mask 0.
	Outputs []string `json:"outputs"`
	Client  string   `json:"client,omitempty"`
}

// Ready is sent when the process is ready, and again after a restart.
type Ready struct {
	Type         string   `json:"type"`
	Version      int      `json:"version"`
	Session      string   `json:"session"`
	Reused       bool     `json:"reused"`
	Restarted    bool     `json:"restarted"`
	Backend      string   `json:"backend"`
	Inputs       []string `json:"inputs"`
	Outputs      []string `json:"outputs"`
	FrameMS      int      `json:"frame_ms"`
	IdleTimeoutS int      `json:"idle_timeout_s"`
	LateAfterMS  int      `json:"late_after_ms"`
}

// Restarting says that the process failed and will start again.
type Restarting struct {
	Type      string `json:"type"`
	Reason    string `json:"reason"`
	RetryInMS int    `json:"retry_in_ms"`
}

// Error codes.
const (
	ErrUnauthorized       = "unauthorized"
	ErrKeyDisabled        = "key_disabled"
	ErrBadHello           = "bad_hello"
	ErrUnsupportedVersion = "unsupported_version"
	ErrUnsupportedCodec   = "unsupported_codec"
	ErrKeyLimit           = "key_limit"
	ErrHostFull           = "host_full"
	ErrBackendStart       = "backend_start_failed"
	ErrBackendFailed      = "backend_failed"
	ErrReplaced           = "replaced"
	ErrShuttingDown       = "shutting_down"
	ErrBadFrameCode       = "bad_frame"
	ErrKeyRevoked         = "key_revoked"
	ErrBacklog            = "backlog"
)

// Error reports a problem. When Fatal is true the server closes the
// connection after it.
type Error struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Fatal   bool   `json:"fatal"`
}

// StatsRequest asks for the stats of the session process.
type StatsRequest struct {
	Type string `json:"type"`
}

// Bye ends the session. With Stop the process stops at once, with no
// idle time.
type Bye struct {
	Type string `json:"type"`
	Stop bool   `json:"stop,omitempty"`
}
