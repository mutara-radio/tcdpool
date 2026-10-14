// Package faketcd is a stand-in for the nostar tcd, for tests. It reads
// a tcd.ini, connects to the reflector port, sends its module letter and
// answers each packet with every requested output filled with test data.
//
// A test can give it commands inside a PCM input frame: USRP[0] is
// CmdMagic and USRP[1] is the command. USRP[2] is an argument.
//
// Environment:
//
//	TCDPOOL_FAKE_PING_MS        ping period (default 5000)
//	TCDPOOL_FAKE_EXIT_AT_START  exit with status 3 before it connects
//	TCDPOOL_FAKE_DELAY_MS       delay before each reply
package faketcd

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dbehnke/tcdpool/internal/backend/nostar"
)

// CmdMagic in USRP[0] marks a command frame.
const CmdMagic = 0x7FCD

// Commands.
const (
	CmdCrash        = 1  // exit at once, with no reply
	CmdSplitWrite   = 2  // write the reply in small pieces
	CmdGarbage      = 3  // write junk bytes before the reply
	CmdWrongModule  = 4  // reply with another module letter
	CmdBadCRC       = 5  // v2: reply with a bad CRC
	CmdWrongVersion = 6  // v2: reply with frame version 9
	CmdHang         = 7  // stop all output (no replies, no pings)
	CmdSlow         = 8  // delay this reply by USRP[2] ms
	CmdUnknownSeq   = 9  // reply with a sequence that was not sent
	CmdBadCodec     = 10 // reply with codec_in 7
)

type config struct {
	port    int
	address string
	module  byte
	v2      bool
}

func readIni(path string) (config, error) {
	c := config{address: "127.0.0.1"}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "Port":
			c.port, _ = strconv.Atoi(v)
		case "ServerAddress":
			c.address = v
		case "Modules":
			if v != "" {
				c.module = v[0]
			}
		case "Framing":
			c.v2 = strings.EqualFold(v, "v2")
		}
	}
	if c.port == 0 || c.module == 0 {
		return c, fmt.Errorf("faketcd: Port or Modules missing in %s", path)
	}
	return c, sc.Err()
}

func envMS(k string, def int) time.Duration {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return time.Duration(v) * time.Millisecond
	}
	return time.Duration(def) * time.Millisecond
}

// Main runs the fake. args[0] is the ini path. It returns the exit code.
func Main(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: faketcd <tcd.ini>")
		return 2
	}
	if os.Getenv("TCDPOOL_FAKE_EXIT_AT_START") != "" {
		fmt.Println("faketcd: exit at start")
		return 3
	}
	cfg, err := readIni(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	conn, err := net.Dial("tcp", net.JoinHostPort(cfg.address, strconv.Itoa(cfg.port)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "faketcd:", err)
		return 1
	}
	fmt.Printf("faketcd: connected, module %c, v2=%v\n", cfg.module, cfg.v2)
	f := &fake{cfg: cfg, conn: conn, delay: envMS("TCDPOOL_FAKE_DELAY_MS", 0)}
	if _, err := conn.Write([]byte{cfg.module}); err != nil {
		return 1
	}
	go f.pinger(envMS("TCDPOOL_FAKE_PING_MS", 5000))
	if err := f.serve(); err != nil {
		fmt.Println("faketcd:", err)
	}
	return 0
}

type fake struct {
	cfg   config
	conn  net.Conn
	delay time.Duration
	mu    sync.Mutex
	hung  bool
}

func (f *fake) write(b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hung {
		return nil
	}
	_, err := f.conn.Write(b)
	return err
}

func (f *fake) frame(p *nostar.Packet, outputs uint8) []byte {
	if f.cfg.v2 {
		b := make([]byte, nostar.V2FrameSize)
		p.EncodeV2(b, outputs)
		return b
	}
	b := make([]byte, nostar.PacketSize)
	p.Encode(b)
	return b
}

func (f *fake) pinger(every time.Duration) {
	for {
		time.Sleep(every)
		p := nostar.Packet{Module: f.cfg.module, CodecIn: nostar.CodecPing}
		if f.write(f.frame(&p, 0)) != nil {
			return
		}
	}
}

func (f *fake) serve() error {
	var rx nostar.V2Reader
	buf := make([]byte, 4096)
	raw := make([]byte, nostar.PacketSize)
	for {
		var p nostar.Packet
		var outputs uint8
		if f.cfg.v2 {
			st, o, why := rx.Next(&p)
			switch st {
			case nostar.V2NeedMore:
				n, err := f.conn.Read(buf)
				if err != nil {
					return err
				}
				rx.Append(buf[:n])
				continue
			case nostar.V2Bad:
				fmt.Println("faketcd: bad frame:", why)
				continue
			case nostar.V2TooManyBad:
				return fmt.Errorf("too many bad frames: %s", why)
			}
			outputs = o
		} else {
			if _, err := io.ReadFull(f.conn, raw); err != nil {
				return err
			}
			if err := p.Decode(raw); err != nil {
				return err
			}
		}
		if p.CodecIn == nostar.CodecPing {
			continue
		}
		if err := f.answer(&p, outputs); err != nil {
			return err
		}
	}
}

// Fill puts test data in every field of p, keeps the input field, and
// then zeroes the fields that are not requested. Tests use it to know
// what a reply holds.
func Fill(p *nostar.Packet, outputs uint8) {
	in := p.CodecIn
	s := byte(p.Sequence)
	fill := func(b []byte, k byte) {
		for i := range b {
			b[i] = s + byte(i)*3 + k | 1
		}
	}
	if in != nostar.CodecDStar {
		fill(p.DStar[:], 0x10)
	}
	if in != nostar.CodecDMR {
		fill(p.DMR[:], 0x20)
	}
	if in != nostar.CodecC23200 && in != nostar.CodecC21600 {
		fill(p.M17[:], 0x30)
	}
	if in != nostar.CodecP25 {
		fill(p.P25[:], 0x40)
	}
	if in != nostar.CodecUSRP {
		for i := range p.USRP {
			p.USRP[i] = int16(int(p.Sequence)*7+i) | 1
		}
	}
	p.KeepOnly(outputs)
}

func (f *fake) answer(p *nostar.Packet, outputs uint8) error {
	cmd, arg := 0, 0
	if p.CodecIn == nostar.CodecUSRP && p.USRP[0] == CmdMagic {
		cmd, arg = int(p.USRP[1]), int(p.USRP[2])
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	mask := nostar.NormalizeOutputs(outputs)
	if !f.cfg.v2 {
		mask = nostar.OutAll
	}
	r := *p
	Fill(&r, mask)
	switch cmd {
	case CmdCrash:
		fmt.Println("faketcd: crash on command")
		os.Exit(2)
	case CmdHang:
		fmt.Println("faketcd: hang on command")
		f.mu.Lock()
		f.hung = true
		f.mu.Unlock()
		return nil
	case CmdSlow:
		time.Sleep(time.Duration(arg) * time.Millisecond)
	case CmdWrongModule:
		r.Module++
	case CmdUnknownSeq:
		r.Sequence += 1000
	case CmdBadCodec:
		b := f.frame(&r, mask)
		off := 8
		if f.cfg.v2 {
			// a v2 frame with a bad codec needs a good CRC to reach the
			// packet check, so build it by hand
			b2 := make([]byte, nostar.PacketSize)
			r.Encode(b2)
			b2[off] = 7
			return f.write(v2Raw(b2, mask))
		}
		b[off] = 7
		return f.write(b)
	}
	b := f.frame(&r, mask)
	switch cmd {
	case CmdSplitWrite:
		for i := 0; i < len(b); i += 50 {
			if err := f.write(b[i:min(i+50, len(b))]); err != nil {
				return err
			}
			time.Sleep(2 * time.Millisecond)
		}
		return nil
	case CmdGarbage:
		if err := f.write([]byte("garbage TC D2 bytes \x00\x01\x02")); err != nil {
			return err
		}
	case CmdBadCRC:
		if f.cfg.v2 {
			b[len(b)-1] ^= 0xFF
		}
	case CmdWrongVersion:
		if f.cfg.v2 {
			b[4] = 9
		}
	}
	return f.write(b)
}

// v2Raw wraps payload bytes in a v2 frame with a good CRC.
func v2Raw(payload []byte, outputs uint8) []byte {
	b := make([]byte, 0, nostar.V2HeaderSize+len(payload)+nostar.V2TrailerSize)
	b = append(b, 'T', 'C', 'D', '2', nostar.V2Version, outputs)
	b = binary.LittleEndian.AppendUint16(b, uint16(len(payload)))
	b = append(b, payload...)
	return binary.LittleEndian.AppendUint32(b, crc32.ChecksumIEEE(b[4:]))
}
