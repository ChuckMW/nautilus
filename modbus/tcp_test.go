package modbus

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

// wire parses "00 01 00 00 ..." golden strings.
func wire(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("bad wire literal %q: %v", s, err)
	}
	return b
}

// scriptRW is a scripted peer for tcpConn: Reads serve the canned response,
// Writes record what the client sent.
type scriptRW struct {
	resp bytes.Reader
	sent bytes.Buffer
}

func newScript(resp []byte) *scriptRW {
	s := &scriptRW{}
	s.resp.Reset(resp)
	return s
}
func (s *scriptRW) Read(p []byte) (int, error)  { return s.resp.Read(p) }
func (s *scriptRW) Write(p []byte) (int, error) { return s.sent.Write(p) }

func TestAppendFrameGolden(t *testing.T) {
	got := appendFrame(nil, 0x0102, 0x11, FCReadHoldingRegisters, []byte{0x00, 0x6B, 0x00, 0x03})
	want := wire(t, "01 02 00 00 00 06 11 03 00 6B 00 03")
	if !bytes.Equal(got, want) {
		t.Fatalf("frame = % X, want % X", got, want)
	}
}

func TestRequestGolden(t *testing.T) {
	// FC3 read of 2 registers from address 0 on unit 1. txid starts at 1.
	s := newScript(wire(t, "00 01 00 00 00 07 01 03 04 00 0A 01 02"))
	c := newTCPConn(s, 0)
	data, err := c.Request(context.Background(), 1, FCReadHoldingRegisters, wire(t, "00 00 00 02"))
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if want := wire(t, "00 01 00 00 00 06 01 03 00 00 00 02"); !bytes.Equal(s.sent.Bytes(), want) {
		t.Errorf("sent % X, want % X", s.sent.Bytes(), want)
	}
	if want := wire(t, "04 00 0A 01 02"); !bytes.Equal(data, want) {
		t.Errorf("data % X, want % X", data, want)
	}
}

func TestRequestErrors(t *testing.T) {
	cases := []struct {
		name string
		resp string
		want string // substring of the error
	}{
		{"exception", "00 01 00 00 00 03 01 83 02", "illegal data address"},
		{"txid mismatch", "00 07 00 00 00 07 01 03 04 00 00 00 00", "transaction id mismatch"},
		{"bad protocol", "00 01 00 07 00 07 01 03 04 00 00 00 00", "protocol id 7"},
		{"short header", "00 01 00 00", "unexpected EOF"},
		{"short body", "00 01 00 00 00 07 01 03 04 00", "unexpected EOF"},
		{"length under", "00 01 00 00 00 01 01", "length 1 out of range"},
		{"wrong unit", "00 01 00 00 00 07 02 03 04 00 00 00 00", "unit id 2"},
		{"wrong fc", "00 01 00 00 00 07 01 04 04 00 00 00 00", "function 0x04"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTCPConn(newScript(wire(t, tc.resp)), 0)
			_, err := c.Request(context.Background(), 1, FCReadHoldingRegisters, wire(t, "00 00 00 02"))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestRequestExceptionTyped(t *testing.T) {
	c := newTCPConn(newScript(wire(t, "00 01 00 00 00 03 01 83 02")), 0)
	_, err := c.Request(context.Background(), 1, FCReadHoldingRegisters, wire(t, "00 00 00 01"))
	var exc ExceptionError
	if !errors.As(err, &exc) || exc.Code != 0x02 {
		t.Fatalf("want ExceptionError{0x02}, got %v", err)
	}
}

func TestRequestShortReadIsUnexpectedEOF(t *testing.T) {
	c := newTCPConn(newScript(wire(t, "00 01 00 00 00 07 01 03")), 0)
	_, err := c.Request(context.Background(), 1, FCReadHoldingRegisters, wire(t, "00 00 00 01"))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want io.ErrUnexpectedEOF, got %v", err)
	}
}

func TestRequestTransactionIDIncrements(t *testing.T) {
	c := newTCPConn(newScript(nil), 0)
	c.rw = newScript(wire(t, "00 01 00 00 00 04 01 03 01 00"+" 00 02 00 00 00 04 01 03 01 00"))
	for i := 1; i <= 2; i++ {
		if _, err := c.Request(context.Background(), 1, FCReadHoldingRegisters, nil); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
}

func TestRequestCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := newTCPConn(newScript(nil), 0)
	if _, err := c.Request(ctx, 1, FCReadHoldingRegisters, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// fakeConn implements the conn seam directly, for the FC helper tests.
type fakeConn struct {
	unit uint8
	fc   byte
	pdu  []byte
	resp []byte
	err  error
}

func (f *fakeConn) Request(_ context.Context, unit uint8, fc byte, pdu []byte) ([]byte, error) {
	f.unit, f.fc, f.pdu = unit, fc, append([]byte(nil), pdu...)
	return f.resp, f.err
}

func TestReadRegistersHelper(t *testing.T) {
	f := &fakeConn{resp: wire(t, "04 12 34 AB CD")}
	regs, err := readRegisters(context.Background(), f, 9, FCReadInputRegisters, 0x0100, 2)
	if err != nil {
		t.Fatalf("readRegisters: %v", err)
	}
	if f.unit != 9 || f.fc != FCReadInputRegisters || !bytes.Equal(f.pdu, wire(t, "01 00 00 02")) {
		t.Errorf("request = unit %d fc %d pdu % X", f.unit, f.fc, f.pdu)
	}
	if len(regs) != 2 || regs[0] != 0x1234 || regs[1] != 0xABCD {
		t.Errorf("regs = %v", regs)
	}
}

func TestReadRegistersBadByteCount(t *testing.T) {
	f := &fakeConn{resp: wire(t, "02 12 34")}
	if _, err := readRegisters(context.Background(), f, 1, FCReadHoldingRegisters, 0, 2); err == nil {
		t.Fatal("want byte-count error, got nil")
	}
}

func TestReadRegistersCountRange(t *testing.T) {
	f := &fakeConn{}
	for _, count := range []uint16{0, 126} {
		if _, err := readRegisters(context.Background(), f, 1, FCReadHoldingRegisters, 0, count); err == nil {
			t.Errorf("count %d: want error", count)
		}
	}
}

func TestReadBitsHelper(t *testing.T) {
	// 10 bits: 0xB5 0x02 -> 1,0,1,0,1,1,0,1  0,1
	f := &fakeConn{resp: wire(t, "02 B5 02")}
	bits, err := readBits(context.Background(), f, 3, FCReadCoils, 20, 10)
	if err != nil {
		t.Fatalf("readBits: %v", err)
	}
	want := []bool{true, false, true, false, true, true, false, true, false, true}
	if len(bits) != len(want) {
		t.Fatalf("got %d bits", len(bits))
	}
	for i := range want {
		if bits[i] != want[i] {
			t.Errorf("bit %d = %v, want %v", i, bits[i], want[i])
		}
	}
	if !bytes.Equal(f.pdu, wire(t, "00 14 00 0A")) {
		t.Errorf("pdu = % X", f.pdu)
	}
}

func TestWriteSingleCoilHelper(t *testing.T) {
	f := &fakeConn{resp: wire(t, "00 05 FF 00")}
	if err := writeSingleCoil(context.Background(), f, 1, 5, true); err != nil {
		t.Fatalf("writeSingleCoil: %v", err)
	}
	if !bytes.Equal(f.pdu, wire(t, "00 05 FF 00")) {
		t.Errorf("pdu = % X", f.pdu)
	}
	// Echo mismatch is an error: the device claims a different write.
	f = &fakeConn{resp: wire(t, "00 05 00 00")}
	if err := writeSingleCoil(context.Background(), f, 1, 5, true); err == nil {
		t.Fatal("want echo mismatch error")
	}
}

func TestWriteSingleRegisterHelper(t *testing.T) {
	f := &fakeConn{resp: wire(t, "00 2A BE EF")}
	if err := writeSingleRegister(context.Background(), f, 1, 42, 0xBEEF); err != nil {
		t.Fatalf("writeSingleRegister: %v", err)
	}
	if f.fc != FCWriteSingleRegister || !bytes.Equal(f.pdu, wire(t, "00 2A BE EF")) {
		t.Errorf("fc %d pdu % X", f.fc, f.pdu)
	}
}

func TestWriteMultipleRegistersHelper(t *testing.T) {
	f := &fakeConn{resp: wire(t, "00 10 00 02")}
	if err := writeMultipleRegisters(context.Background(), f, 1, 16, []uint16{0x0102, 0x0304}); err != nil {
		t.Fatalf("writeMultipleRegisters: %v", err)
	}
	if !bytes.Equal(f.pdu, wire(t, "00 10 00 02 04 01 02 03 04")) {
		t.Errorf("pdu = % X", f.pdu)
	}
	if err := writeMultipleRegisters(context.Background(), f, 1, 0, nil); err == nil {
		t.Error("empty write: want error")
	}
	if err := writeMultipleRegisters(context.Background(), f, 1, 0, make([]uint16, 124)); err == nil {
		t.Error("oversize write: want error")
	}
}

func TestWriteMultipleCoilsHelper(t *testing.T) {
	f := &fakeConn{resp: wire(t, "00 13 00 0A")}
	values := []bool{true, false, true, true, false, false, true, true, true, false}
	if err := writeMultipleCoils(context.Background(), f, 1, 19, values); err != nil {
		t.Fatalf("writeMultipleCoils: %v", err)
	}
	if !bytes.Equal(f.pdu, wire(t, "00 13 00 0A 02 CD 01")) {
		t.Errorf("pdu = % X", f.pdu)
	}
}

func TestPackUnpackBitsRoundTrip(t *testing.T) {
	for _, n := range []int{1, 7, 8, 9, 16, 100} {
		in := make([]bool, n)
		for i := range in {
			in[i] = i%3 == 0
		}
		out := unpackBits(packBits(in), n)
		for i := range in {
			if out[i] != in[i] {
				t.Fatalf("n=%d bit %d: got %v", n, i, out[i])
			}
		}
	}
}

// A response carrying a different unit id than the request is NOT the
// device's answer to our question — a gateway that routed the wrong drop,
// or a desynced stream. It is rejected as a TRANSPORT error (reconnect),
// never mistaken for an exception (per-block failure) or for data.
func TestRequestWrongUnitIDIsTransportError(t *testing.T) {
	// FC3 to unit 1; the frame that comes back says unit 2.
	c := newTCPConn(newScript(wire(t, "00 01 00 00 00 07 02 03 04 00 0A 01 02")), 0)
	data, err := c.Request(context.Background(), 1, FCReadHoldingRegisters, wire(t, "00 00 00 02"))
	if err == nil {
		t.Fatalf("want error, got data % X", data)
	}
	if data != nil {
		t.Errorf("no data may be returned with the error, got % X", data)
	}
	var exc ExceptionError
	if errors.As(err, &exc) {
		t.Errorf("wrong unit id must not be an ExceptionError, got %v", err)
	}
	if want := "response unit id 2 (want 1)"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the unit mismatch %q", err, want)
	}
}
