// slave_roundtrip_test.go runs the real client against the in-process slave
// over 127.0.0.1 — every function code on the wire, plus the failure modes
// (exception injection, unknown unit, latency-forced timeout). No build
// tags, no environment gating: the eip/logixserver precedent.
package modbus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/joyautomation/nautilus/modbus/slave"
)

// startSlave brings a slave up on an ephemeral port and dials it.
func startSlave(t *testing.T) (*slave.Server, *tcpConn) {
	t.Helper()
	srv := slave.NewServer("", nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("slave start: %v", err)
	}
	t.Cleanup(srv.Stop)
	c, err := dialTCP(context.Background(), srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", srv.Addr(), err)
	}
	t.Cleanup(func() { c.Close() })
	return srv, c
}

func TestSlaveRoundTripReads(t *testing.T) {
	srv, c := startSlave(t)
	ctx := context.Background()
	u := srv.Unit(1)
	u.SetHolding(0, 0x1234)
	u.SetHolding(1, 0xABCD)
	u.SetInput(5, 42)
	u.SetCoil(3, true)
	u.SetDiscrete(7, true)

	regs, err := readRegisters(ctx, c, 1, FCReadHoldingRegisters, 0, 3)
	if err != nil {
		t.Fatalf("FC3: %v", err)
	}
	if regs[0] != 0x1234 || regs[1] != 0xABCD || regs[2] != 0 {
		t.Errorf("FC3 = %04X", regs)
	}

	regs, err = readRegisters(ctx, c, 1, FCReadInputRegisters, 4, 2)
	if err != nil {
		t.Fatalf("FC4: %v", err)
	}
	if regs[0] != 0 || regs[1] != 42 {
		t.Errorf("FC4 = %v", regs)
	}

	bits, err := readBits(ctx, c, 1, FCReadCoils, 0, 10)
	if err != nil {
		t.Fatalf("FC1: %v", err)
	}
	if !bits[3] || bits[0] || bits[9] {
		t.Errorf("FC1 = %v", bits)
	}

	bits, err = readBits(ctx, c, 1, FCReadDiscreteInputs, 7, 1)
	if err != nil {
		t.Fatalf("FC2: %v", err)
	}
	if !bits[0] {
		t.Errorf("FC2 = %v", bits)
	}
}

func TestSlaveRoundTripWrites(t *testing.T) {
	srv, c := startSlave(t)
	ctx := context.Background()
	u := srv.Unit(1)

	if err := writeSingleRegister(ctx, c, 1, 100, 0xBEEF); err != nil {
		t.Fatalf("FC6: %v", err)
	}
	if got := u.Holding(100); got != 0xBEEF {
		t.Errorf("FC6 landed %04X", got)
	}

	if err := writeSingleCoil(ctx, c, 1, 9, true); err != nil {
		t.Fatalf("FC5: %v", err)
	}
	if !u.Coil(9) {
		t.Error("FC5 did not set the coil")
	}
	if err := writeSingleCoil(ctx, c, 1, 9, false); err != nil {
		t.Fatalf("FC5 off: %v", err)
	}
	if u.Coil(9) {
		t.Error("FC5 did not clear the coil")
	}

	if err := writeMultipleRegisters(ctx, c, 1, 200, []uint16{1, 2, 3}); err != nil {
		t.Fatalf("FC16: %v", err)
	}
	for i, want := range []uint16{1, 2, 3} {
		if got := u.Holding(200 + uint16(i)); got != want {
			t.Errorf("FC16 reg %d = %d, want %d", 200+i, got, want)
		}
	}

	values := []bool{true, false, true, true, false, false, true, true, true}
	if err := writeMultipleCoils(ctx, c, 1, 50, values); err != nil {
		t.Fatalf("FC15: %v", err)
	}
	for i, want := range values {
		if got := u.Coil(50 + uint16(i)); got != want {
			t.Errorf("FC15 coil %d = %v, want %v", 50+i, got, want)
		}
	}
}

// Reads and writes round-trip through encode.go the way the driver will use
// them: engineering value in, registers on the wire, engineering value out.
func TestSlaveEncodeDecodeEndToEnd(t *testing.T) {
	srv, c := startSlave(t)
	ctx := context.Background()
	srv.Unit(1) // register the unit

	f := mustFormat(t, "float32")
	regs, err := encodeRegisters(f, 123.5, OrderLittle, "", 0, 0)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := writeMultipleRegisters(ctx, c, 1, 14, regs); err != nil {
		t.Fatalf("FC16: %v", err)
	}
	back, err := readRegisters(ctx, c, 1, FCReadHoldingRegisters, 14, 2)
	if err != nil {
		t.Fatalf("FC3: %v", err)
	}
	got, err := decodeRegisters(f, back, OrderLittle, "", 0, 0)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != 123.5 {
		t.Errorf("round trip = %v", got)
	}
}

// One listener, several unit-ids — the Anybus-gateway shape.
func TestSlaveMultiUnit(t *testing.T) {
	srv, c := startSlave(t)
	ctx := context.Background()
	srv.Unit(1).SetHolding(0, 111)
	srv.Unit(2).SetHolding(0, 222)

	for unit, want := range map[uint8]uint16{1: 111, 2: 222} {
		regs, err := readRegisters(ctx, c, unit, FCReadHoldingRegisters, 0, 1)
		if err != nil {
			t.Fatalf("unit %d: %v", unit, err)
		}
		if regs[0] != want {
			t.Errorf("unit %d = %d, want %d", unit, regs[0], want)
		}
	}

	// A unit nobody created answers like an absent gateway drop.
	_, err := readRegisters(ctx, c, 9, FCReadHoldingRegisters, 0, 1)
	var exc ExceptionError
	if !errors.As(err, &exc) || exc.Code != 0x0B {
		t.Fatalf("unknown unit: %v", err)
	}
}

func TestSlaveExceptionInjection(t *testing.T) {
	srv, c := startSlave(t)
	ctx := context.Background()
	u := srv.Unit(1)
	u.SetHolding(0, 7)
	u.InjectException(FCReadHoldingRegisters, 0x02)

	_, err := readRegisters(ctx, c, 1, FCReadHoldingRegisters, 0, 1)
	var exc ExceptionError
	if !errors.As(err, &exc) || exc.Code != 0x02 {
		t.Fatalf("want ExceptionError{0x02}, got %v", err)
	}
	// The exception is per function code: writes still work — the shape of
	// FTIR's reg-46 collision, where one block faults and the rest lives.
	if err := writeSingleRegister(ctx, c, 1, 5, 1); err != nil {
		t.Fatalf("FC6 during FC3 injection: %v", err)
	}
	// Clearing restores the read.
	u.InjectException(FCReadHoldingRegisters, 0)
	regs, err := readRegisters(ctx, c, 1, FCReadHoldingRegisters, 0, 1)
	if err != nil || regs[0] != 7 {
		t.Fatalf("after clear: %v %v", regs, err)
	}
}

// Artificial latency past the client timeout reads as a transport error —
// the driver's cue to reconnect with backoff.
func TestSlaveLatencyForcesTimeout(t *testing.T) {
	srv := slave.NewServer("", nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("slave start: %v", err)
	}
	defer srv.Stop()
	srv.Unit(1)
	srv.SetLatency(500 * time.Millisecond)

	c, err := dialTCP(context.Background(), srv.Addr(), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_, err = readRegisters(context.Background(), c, 1, FCReadHoldingRegisters, 0, 1)
	if err == nil {
		t.Fatal("want timeout error")
	}
	var exc ExceptionError
	if errors.As(err, &exc) {
		t.Fatalf("timeout must not look like a device exception: %v", err)
	}
}
