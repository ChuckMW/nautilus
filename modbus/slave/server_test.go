package slave

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// rawRequest speaks the wire by hand — the slave must hold up against a
// client that is not our own.
func rawRequest(t *testing.T, c net.Conn, txid uint16, unit, fc byte, data []byte) (rfc byte, rdata []byte) {
	t.Helper()
	req := make([]byte, 8+len(data))
	binary.BigEndian.PutUint16(req[0:2], txid)
	binary.BigEndian.PutUint16(req[4:6], uint16(2+len(data)))
	req[6], req[7] = unit, fc
	copy(req[8:], data)
	if _, err := c.Write(req); err != nil {
		t.Fatalf("write: %v", err)
	}
	var hdr [7]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		t.Fatalf("read header: %v", err)
	}
	if got := binary.BigEndian.Uint16(hdr[0:2]); got != txid {
		t.Fatalf("txid echo = %d, want %d", got, txid)
	}
	body := make([]byte, binary.BigEndian.Uint16(hdr[4:6])-1)
	if _, err := io.ReadFull(c, body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body[0], body[1:]
}

func startRaw(t *testing.T) (*Server, net.Conn) {
	t.Helper()
	srv := NewServer("", nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(srv.Stop)
	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return srv, c
}

func TestUnknownUnitAnswersGatewayException(t *testing.T) {
	_, c := startRaw(t)
	fc, data := rawRequest(t, c, 1, 7, 0x03, []byte{0, 0, 0, 1})
	if fc != 0x83 || len(data) != 1 || data[0] != 0x0B {
		t.Fatalf("response fc %02X data % X", fc, data)
	}
}

func TestUnsupportedFunctionAnswersIllegalFunction(t *testing.T) {
	srv, c := startRaw(t)
	srv.Unit(1)
	fc, data := rawRequest(t, c, 1, 1, 0x2B, []byte{0x0E, 0x01, 0x00})
	if fc != 0x2B|0x80 || data[0] != 0x01 {
		t.Fatalf("response fc %02X data % X", fc, data)
	}
}

func TestMalformedCountAnswersIllegalDataValue(t *testing.T) {
	srv, c := startRaw(t)
	srv.Unit(1)
	// FC3 count 0 and count 126 are both out of the spec's 1..125.
	for _, count := range []uint16{0, 126} {
		fc, data := rawRequest(t, c, 1, 1, 0x03, []byte{0, 0, byte(count >> 8), byte(count)})
		if fc != 0x83 || data[0] != 0x03 {
			t.Fatalf("count %d: fc %02X data % X", count, fc, data)
		}
	}
}

func TestBadProtocolIDDropsConnection(t *testing.T) {
	srv, c := startRaw(t)
	srv.Unit(1)
	req := []byte{0, 1, 0, 9 /* protocol id 9 */, 0, 6, 1, 3, 0, 0, 0, 1}
	if _, err := c.Write(req); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The server hangs up without answering: EOF, or a reset when it closes
	// before draining the request — either way, no response frame.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	var one [1]byte
	if n, err := c.Read(one[:]); err == nil {
		t.Fatalf("want dropped connection, got %d response bytes", n)
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("connection neither answered nor dropped: %v", err)
	}
}

func TestStoreAccessorsAndConcurrency(t *testing.T) {
	srv := NewServer("", nil)
	u := srv.Unit(1)
	if srv.Unit(1) != u {
		t.Fatal("Unit must be stable per id")
	}
	u.SetHolding(1, 11)
	u.SetInput(2, 22)
	u.SetCoil(3, true)
	u.SetDiscrete(4, true)
	if u.Holding(1) != 11 || u.Input(2) != 22 || !u.Coil(3) || !u.Discrete(4) {
		t.Fatal("accessors disagree with setters")
	}
	if u.Holding(99) != 0 || u.Coil(99) {
		t.Fatal("unset addresses must read zero/false")
	}
	// Hammer the store from several goroutines — the race detector is the
	// assertion here.
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				u.SetHolding(uint16(i), uint16(g))
				_ = u.Holding(uint16(i))
				u.SetCoil(uint16(i), i%2 == 0)
				_ = u.Coil(uint16(i))
			}
		}()
	}
	wg.Wait()
}
