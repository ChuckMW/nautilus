// driver_test.go is the D1 acceptance suite: the whole driver against the
// in-process slave over 127.0.0.1 — no build tags, no environment gating
// (the eip/logixserver precedent). Wire-level assertions ride a recording
// dialer that parses every outbound MBAP frame, so "a write became FC16"
// is checked on the actual bytes.
package modbus

import (
	"context"
	"encoding/binary"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	nio "github.com/joyautomation/nautilus/io"
	"github.com/joyautomation/nautilus/modbus/slave"
)

// fastSource returns a source aimed at the test slave with test-speed
// retry/backoff.
func fastSource(id, addr string) Source {
	host, port, _ := net.SplitHostPort(addr)
	var p int
	for _, c := range port {
		p = p*10 + int(c-'0')
	}
	return Source{
		ID: id, Host: host, Port: p, UnitID: 1,
		Timeout:  500 * time.Millisecond,
		RetryMin: 20 * time.Millisecond,
		RetryMax: 80 * time.Millisecond,
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// frameRec is one outbound request as it hit the wire.
type frameRec struct {
	fc    byte
	addr  uint16
	count uint16 // count for reads/multi-writes, value for single writes
}

// recorder collects outbound frames across reconnects.
type recorder struct {
	mu     sync.Mutex
	frames []frameRec
}

func (r *recorder) add(f frameRec) {
	r.mu.Lock()
	r.frames = append(r.frames, f)
	r.mu.Unlock()
}

// writes returns the recorded frames for one write function code, or for
// (fc, addr) when addr ≥ 0.
func (r *recorder) writes(fc byte, addr int) []frameRec {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []frameRec
	for _, f := range r.frames {
		if f.fc == fc && (addr < 0 || int(f.addr) == addr) {
			out = append(out, f)
		}
	}
	return out
}

// recordingConn wraps a net.Conn and parses each outbound ADU. The driver
// writes one whole frame per Write call (appendFrame), so parsing is exact.
type recordingConn struct {
	net.Conn
	rec *recorder
}

func (c *recordingConn) Write(b []byte) (int, error) {
	if len(b) >= 12 {
		c.rec.add(frameRec{
			fc:    b[7],
			addr:  binary.BigEndian.Uint16(b[8:10]),
			count: binary.BigEndian.Uint16(b[10:12]),
		})
	}
	return c.Conn.Write(b)
}

// recordingDialer dials for real and wraps the stream.
func recordingDialer(rec *recorder) func(ctx context.Context, addr string) (net.Conn, error) {
	return func(ctx context.Context, addr string) (net.Conn, error) {
		var d net.Dialer
		nc, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		return &recordingConn{Conn: nc, rec: rec}, nil
	}
}

func startDriver(t *testing.T, d *Driver) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.Start(ctx)
	t.Cleanup(d.Stop)
}

func newSlave(t *testing.T) *slave.Server {
	t.Helper()
	srv := slave.NewServer("", nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("slave start: %v", err)
	}
	t.Cleanup(srv.Stop)
	return srv
}

// ── delivery ─────────────────────────────────────────────────────────────

// Tags land, per scan class, across every table, decoded and scaled; the
// companions and name lists say what the driver serves.
func TestDriverDeliversTagsPerClass(t *testing.T) {
	srv := newSlave(t)
	u := srv.Unit(1)
	u.SetHolding(0, 0xFFF6) // int16 -10, scale 0.1 → -1.0
	regs, _ := encodeRegisters(Format{Kind: "float32"}, 123.5, "", "", 0, 0)
	u.SetHolding(14, regs[0])
	u.SetHolding(15, regs[1])
	u.SetInput(5, 42)
	u.SetCoil(2, true)
	u.SetDiscrete(4, true)

	m := Manifest{
		Sources: []Source{fastSource("DEV", srv.Addr())},
		Tags: []TagBinding{
			{Name: "T_Temp", Source: "DEV", Table: TableHolding, Address: 0, Format: "int16", Scale: 0.1},
			{Name: "T_CO", Source: "DEV", Table: TableHolding, Address: 14, Format: "float32"},
			{Name: "T_Raw", Source: "DEV", Table: TableInput, Address: 5, Format: "uint16", ScanClass: "slow"},
			{Name: "T_Run", Source: "DEV", Table: TableCoil, Address: 2},
			{Name: "T_Flt", Source: "DEV", Table: TableDiscrete, Address: 4},
		},
	}
	d, err := New(m, WithScanRate(20*time.Millisecond), WithScanClass("slow", 60*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}

	wantIn := []string{"DEV__Online", "T_CO", "T_Flt", "T_Raw", "T_Run", "T_Temp"}
	if got := d.InputNames(); !reflect.DeepEqual(got, wantIn) {
		t.Fatalf("InputNames = %v, want %v", got, wantIn)
	}
	classes := d.ScanClasses()
	if !reflect.DeepEqual(classes["slow"], []string{"T_Raw"}) {
		t.Fatalf("ScanClasses[slow] = %v", classes["slow"])
	}
	if len(classes[DefaultClass]) != 4 {
		t.Fatalf("ScanClasses[default] = %v", classes[DefaultClass])
	}

	startDriver(t, d)
	var vals nio.Values
	waitFor(t, "all tags delivered", func() bool {
		v, err := d.ReadInputs()
		if err != nil {
			return false
		}
		vals = v
		return v["T_Temp"] != nil && v["T_CO"] != nil && v["T_Raw"] != nil &&
			v["T_Run"] != nil && v["T_Flt"] != nil && v["DEV__Online"] == true
	})
	if got := vals["T_Temp"]; got != -1.0 {
		t.Errorf("T_Temp = %v, want -1.0", got)
	}
	if got := vals["T_CO"]; got != 123.5 {
		t.Errorf("T_CO = %v", got)
	}
	if got := vals["T_Raw"]; got != int64(42) {
		t.Errorf("T_Raw = %v (%T)", got, got)
	}
	if vals["T_Run"] != true || vals["T_Flt"] != true {
		t.Errorf("bits: run=%v flt=%v", vals["T_Run"], vals["T_Flt"])
	}
	if q := d.Quality(); len(q) != 0 {
		t.Errorf("healthy source must report no non-Good quality, got %v", q)
	}

	// The BatchReader form delivers the same set into the caller's map,
	// and clears what the driver does not hold.
	dst := nio.Values{"Stray": 1}
	if err := d.ReadInputsInto(dst); err != nil {
		t.Fatal(err)
	}
	if _, ok := dst["Stray"]; ok {
		t.Error("ReadInputsInto must drop keys the driver does not hold")
	}
	if dst["T_CO"] != 123.5 || dst["DEV__Online"] != true {
		t.Errorf("ReadInputsInto delivery: %v", dst)
	}

	h := d.Health()
	if len(h.Sources) != 1 || h.Sources[0].State != "connected" || h.Sources[0].Blocks == 0 {
		t.Errorf("Health = %+v", h)
	}
}

// New validates offline and never dials; reads before Start fail loudly.
func TestNewNeverDialsAndReadsFaultBeforeStart(t *testing.T) {
	dialed := false
	m := Manifest{
		Sources: []Source{{ID: "S", Host: "192.0.2.1"}},
		Tags:    []TagBinding{{Name: "A", Source: "S", Table: TableHolding, Address: 0, Format: "uint16"}},
	}
	d, err := New(m, WithDialer(func(ctx context.Context, addr string) (net.Conn, error) {
		dialed = true
		return nil, context.Canceled
	}))
	if err != nil {
		t.Fatal(err)
	}
	if dialed {
		t.Fatal("New must never dial")
	}
	if _, err := d.ReadInputs(); err == nil {
		t.Fatal("ReadInputs before Start must error")
	}

	// And a broken manifest fails in New, offline.
	bad := Manifest{Tags: []TagBinding{{Name: "A", Source: "nope", Table: TableHolding, Format: "uint16"}}}
	if _, err := New(bad); err == nil {
		t.Fatal("New must reject an invalid manifest")
	}
}

// ── writes ───────────────────────────────────────────────────────────────

// A changed output becomes exactly the right function code on the wire:
// contiguous holding registers coalesce to FC16, a lone register is FC6, a
// lone coil FC5, contiguous coils FC15 — and the first snapshot after Start
// is a baseline that writes NOTHING.
func TestWriteCoalescingAndBaseline(t *testing.T) {
	srv := newSlave(t)
	u := srv.Unit(1)
	rec := &recorder{}

	m := Manifest{
		Sources: []Source{fastSource("DEV", srv.Addr())},
		Tags: []TagBinding{
			{Name: "W_A", Source: "DEV", Table: TableHolding, Address: 200, Format: "uint16", Writable: true},
			{Name: "W_B", Source: "DEV", Table: TableHolding, Address: 201, Format: "uint16", Writable: true},
			{Name: "W_C", Source: "DEV", Table: TableHolding, Address: 300, Format: "uint16", Writable: true},
			{Name: "W_D", Source: "DEV", Table: TableCoil, Address: 10, Writable: true},
			{Name: "W_E", Source: "DEV", Table: TableCoil, Address: 11, Writable: true},
			{Name: "W_F", Source: "DEV", Table: TableCoil, Address: 50, Writable: true},
		},
	}
	d, err := New(m, WithScanRate(20*time.Millisecond), WithDialer(recordingDialer(rec)))
	if err != nil {
		t.Fatal(err)
	}
	wantOut := []string{"W_A", "W_B", "W_C", "W_D", "W_E", "W_F"}
	if got := d.OutputNames(); !reflect.DeepEqual(got, wantOut) {
		t.Fatalf("OutputNames = %v", got)
	}
	startDriver(t, d)
	waitFor(t, "connect", func() bool { return d.Health().Sources[0].State == "connected" })

	// Baseline: the state of the world at t=0, not a set of commands.
	base := nio.Values{"W_A": float64(1), "W_B": float64(2), "W_C": float64(3),
		"W_D": false, "W_E": false, "W_F": false}
	if err := d.WriteOutputs(base); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	for _, fc := range []byte{FCWriteSingleRegister, FCWriteMultipleRegisters, FCWriteSingleCoil, FCWriteMultipleCoils} {
		if got := rec.writes(fc, -1); len(got) != 0 {
			t.Fatalf("baseline must write nothing; saw FC%d frames %v", fc, got)
		}
	}

	// Real commands: every value moved.
	if err := d.WriteOutputs(nio.Values{"W_A": float64(11), "W_B": float64(22),
		"W_C": float64(33), "W_D": true, "W_E": true, "W_F": true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "writes to land", func() bool {
		return u.Holding(200) == 11 && u.Holding(201) == 22 && u.Holding(300) == 33 &&
			u.Coil(10) && u.Coil(11) && u.Coil(50)
	})

	if got := rec.writes(FCWriteMultipleRegisters, 200); len(got) != 1 || got[0].count != 2 {
		t.Errorf("W_A+W_B must be one FC16 of 2 registers at 200, got %v", got)
	}
	if got := rec.writes(FCWriteSingleRegister, 300); len(got) != 1 {
		t.Errorf("W_C must be one FC6 at 300, got %v", got)
	}
	if got := rec.writes(FCWriteMultipleCoils, 10); len(got) != 1 || got[0].count != 2 {
		t.Errorf("W_D+W_E must be one FC15 of 2 coils at 10, got %v", got)
	}
	if got := rec.writes(FCWriteSingleCoil, 50); len(got) != 1 {
		t.Errorf("W_F must be one FC5 at 50, got %v", got)
	}

	// Change-only: handing the same values again writes nothing more.
	before := len(rec.writes(FCWriteSingleRegister, 300))
	if err := d.WriteOutputs(nio.Values{"W_C": float64(33)}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if after := len(rec.writes(FCWriteSingleRegister, 300)); after != before {
		t.Errorf("unchanged value must not re-write: %d → %d frames", before, after)
	}
}

// Rewrite re-asserts the last commanded value on its period and only there
// — the non-rewrite binding stays change-only — and a closed write gate
// stops the re-asserts so a device-side watchdog can trip.
func TestRewriteFiresOnPeriodAndStopsOnGate(t *testing.T) {
	srv := newSlave(t)
	u := srv.Unit(1)
	rec := &recorder{}

	m := Manifest{
		Sources: []Source{fastSource("DEV", srv.Addr())},
		Tags: []TagBinding{
			{Name: "KA", Source: "DEV", Table: TableHolding, Address: 400, Format: "uint16", Writable: true, Rewrite: 30 * time.Millisecond},
			{Name: "SP", Source: "DEV", Table: TableHolding, Address: 500, Format: "uint16", Writable: true},
		},
	}
	d, err := New(m, WithScanRate(20*time.Millisecond), WithDialer(recordingDialer(rec)))
	if err != nil {
		t.Fatal(err)
	}
	startDriver(t, d)
	waitFor(t, "connect", func() bool { return d.Health().Sources[0].State == "connected" })

	// Baseline: the keep-alive binding IS written on connect (an SC10 word
	// must be asserted); the plain binding is not.
	if err := d.WriteOutputs(nio.Values{"KA": float64(7), "SP": float64(9)}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "keep-alive baseline write", func() bool { return u.Holding(400) == 7 })
	waitFor(t, "keep-alive re-asserts", func() bool {
		return len(rec.writes(FCWriteSingleRegister, 400)) >= 4
	})
	if got := rec.writes(FCWriteSingleRegister, 500); len(got) != 0 {
		t.Fatalf("non-rewrite binding must not be written at baseline, got %v", got)
	}
	if u.Holding(500) != 0 {
		t.Fatal("baseline leaked a write to the plain binding")
	}

	// The plain binding writes once per change, never again.
	if err := d.WriteOutputs(nio.Values{"SP": float64(10)}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "SP write", func() bool { return u.Holding(500) == 10 })
	time.Sleep(100 * time.Millisecond)
	if got := rec.writes(FCWriteSingleRegister, 500); len(got) != 1 {
		t.Errorf("plain binding must write exactly once per change, got %d frames", len(got))
	}

	// Close the gate: re-asserts stop (the device watchdog would now trip,
	// by design). Reopen: they resume.
	d.SetWriteGate(func() bool { return false })
	time.Sleep(60 * time.Millisecond) // drain any in-flight fire
	quiet := len(rec.writes(FCWriteSingleRegister, 400))
	time.Sleep(150 * time.Millisecond)
	if now := len(rec.writes(FCWriteSingleRegister, 400)); now != quiet {
		t.Errorf("closed gate must stop re-asserts: %d → %d frames", quiet, now)
	}
	d.SetWriteGate(func() bool { return true })
	waitFor(t, "re-asserts resume", func() bool {
		return len(rec.writes(FCWriteSingleRegister, 400)) > quiet
	})
}

// ── failure & recovery ───────────────────────────────────────────────────

// Killing the slave marks the source's tags Stale and __Online false while
// the VALUES HOLD and reads stay scan-safe; a write issued while dead is
// queued (last value per tag) and delivered on reconnect, and quality
// recovers.
func TestSlaveDeathHoldsValuesQueuesWritesRecovers(t *testing.T) {
	srv := newSlave(t)
	srv.Unit(1).SetHolding(0, 21)
	addr := srv.Addr()

	m := Manifest{
		Sources: []Source{fastSource("DEV", addr)},
		Tags: []TagBinding{
			{Name: "PV", Source: "DEV", Table: TableHolding, Address: 0, Format: "uint16"},
			{Name: "OUT", Source: "DEV", Table: TableHolding, Address: 8, Format: "uint16", Writable: true},
		},
	}
	d, err := New(m, WithScanRate(15*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	startDriver(t, d)
	waitFor(t, "first delivery", func() bool {
		v, _ := d.ReadInputs()
		return v != nil && v["PV"] == int64(21)
	})
	if err := d.WriteOutputs(nio.Values{"OUT": float64(1)}); err != nil { // baseline
		t.Fatal(err)
	}

	srv.Stop()
	waitFor(t, "source reported dead", func() bool {
		v, err := d.ReadInputs() // never faults the scan
		if err != nil {
			t.Fatalf("ReadInputs must stay scan-safe while dead: %v", err)
		}
		return v["DEV__Online"] == false && d.Quality()["PV"] == nio.Stale
	})
	v, _ := d.ReadInputs()
	if v["PV"] != int64(21) {
		t.Fatalf("values must hold while dead, PV = %v", v["PV"])
	}
	if _, ok := v["OUT"]; ok {
		// OUT was polled too; it may or may not have landed before the
		// death — either way it must not be zeroed.
		if v["OUT"] != int64(0) && v["OUT"] == nil {
			t.Fatalf("held OUT = %v", v["OUT"])
		}
	}

	// A command issued into the darkness queues, and Health says so.
	if err := d.WriteOutputs(nio.Values{"OUT": float64(77)}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "queued write reported", func() bool {
		h := d.Health()
		return len(h.Sources) == 1 && h.Sources[0].QueuedWrites == 1
	})

	// Same address, new slave: the driver reconnects, delivers the queued
	// write, and quality returns to Good.
	srv2 := slave.NewServer(addr, nil)
	if err := srv2.Start(); err != nil {
		t.Fatalf("slave restart: %v", err)
	}
	t.Cleanup(srv2.Stop)
	srv2.Unit(1).SetHolding(0, 22)

	waitFor(t, "recovery", func() bool {
		v, _ := d.ReadInputs()
		return v["DEV__Online"] == true && v["PV"] == int64(22)
	})
	waitFor(t, "queued write delivered", func() bool {
		return srv2.Unit(1).Holding(8) == 77
	})
	if q := d.Quality(); q["PV"] != nio.Good || len(q) != 0 {
		t.Errorf("recovered source must be all-Good, got %v", q)
	}
}

// An exception on one block turns exactly that block's tags Bad while the
// rest of the source keeps polling Good; clearing the fault recovers.
func TestExceptionOnOneBlockLeavesOthersGood(t *testing.T) {
	srv := newSlave(t)
	u := srv.Unit(1)
	u.SetHolding(0, 5)
	u.SetInput(0, 6)
	u.InjectException(FCReadInputRegisters, 0x02) // FTIR's reg-46 shape

	m := Manifest{
		Sources: []Source{fastSource("DEV", srv.Addr())},
		Tags: []TagBinding{
			{Name: "OK", Source: "DEV", Table: TableHolding, Address: 0, Format: "uint16"},
			{Name: "BAD", Source: "DEV", Table: TableInput, Address: 0, Format: "uint16"},
		},
	}
	d, err := New(m, WithScanRate(15*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	startDriver(t, d)

	waitFor(t, "good block delivers, bad block marked", func() bool {
		v, _ := d.ReadInputs()
		q := d.Quality()
		return v != nil && v["OK"] == int64(5) && q["BAD"] == nio.Bad && q["OK"] == nio.Good
	})
	waitFor(t, "block parks after consecutive exceptions", func() bool {
		h := d.Health().Sources[0]
		return h.Exceptions >= exceptionsToPark && h.BadBlocks == 1
	})
	if v, _ := d.ReadInputs(); v["DEV__Online"] != true {
		t.Error("an exception-parked block must not take the source offline")
	}

	u.InjectException(FCReadInputRegisters, 0)
	waitFor(t, "block recovers after the fault clears", func() bool {
		v, _ := d.ReadInputs()
		return v["BAD"] == int64(6) && len(d.Quality()) == 0
	})
}

// The Enable tag parks the source: connection closed, __Online false, tags
// Stale, no polling — and re-enabling reconnects.
func TestEnableTagParksSource(t *testing.T) {
	srv := newSlave(t)
	srv.Unit(1).SetHolding(0, 9)

	src := fastSource("DEV", srv.Addr())
	src.Enable = "DEV_En"
	m := Manifest{
		Sources: []Source{src},
		Tags: []TagBinding{
			{Name: "PV", Source: "DEV", Table: TableHolding, Address: 0, Format: "uint16"},
		},
	}
	d, err := New(m, WithScanRate(15*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.OutputNames(); !reflect.DeepEqual(got, []string{"DEV_En"}) {
		t.Fatalf("OutputNames = %v — the enable tag is a driver command", got)
	}
	startDriver(t, d)
	waitFor(t, "connect", func() bool {
		v, _ := d.ReadInputs()
		return v["DEV_En"] == nil && v["PV"] == int64(9) && v["DEV__Online"] == true
	})

	if err := d.WriteOutputs(nio.Values{"DEV_En": false}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "parked", func() bool {
		h := d.Health().Sources[0]
		v, _ := d.ReadInputs()
		return h.State == "parked" && v["DEV__Online"] == false && d.Quality()["PV"] == nio.Stale
	})
	if v, _ := d.ReadInputs(); v["PV"] != int64(9) {
		t.Error("parked source must hold its last values")
	}

	if err := d.WriteOutputs(nio.Values{"DEV_En": true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "unparked", func() bool {
		h := d.Health().Sources[0]
		v, _ := d.ReadInputs()
		return h.State == "connected" && v["DEV__Online"] == true && len(d.Quality()) == 0
	})
}
