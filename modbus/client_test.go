// client_test.go pins the exported tooling surface — Client, Decode, Encode
// — against the in-process slave. The internals are covered by
// slave_roundtrip_test.go; what matters here is that the exported seam the
// CLI builds on stays wired to the same codec the driver uses.
package modbus

import (
	"context"
	"testing"
	"time"

	"github.com/joyautomation/nautilus/modbus/slave"
)

func TestClientReadsAndDecodes(t *testing.T) {
	srv := slave.NewServer("", nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("slave start: %v", err)
	}
	t.Cleanup(srv.Stop)

	f, err := ParseFormat("float32")
	if err != nil {
		t.Fatal(err)
	}
	// Seed 12.5 word-swapped, the way an Anybus-fronted device stores it.
	regs, err := Encode(f, 12.5, OrderLittle, "", 0, 0)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	u := srv.Unit(3)
	for i, r := range regs {
		u.SetHolding(uint16(10+i), r)
	}
	u.SetCoil(7, true)

	cl, err := Dial(context.Background(), srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { cl.Close() })

	ctx := context.Background()
	got, err := cl.ReadRegisters(ctx, 3, TableHolding, 10, 2)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	v, err := Decode(f, got, OrderLittle, "", 0, 0)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v != 12.5 {
		t.Errorf("decoded %v, want 12.5", v)
	}
	// The same registers read as big word order must NOT be 12.5 — the
	// order actually matters, or this test proves nothing.
	if wrong, _ := Decode(f, got, OrderBig, "", 0, 0); wrong == v {
		t.Errorf("word order ignored: big and little both decode %v", v)
	}

	bits, err := cl.ReadBits(ctx, 3, TableCoil, 7, 1)
	if err != nil {
		t.Fatalf("read bits: %v", err)
	}
	if !bits[0] {
		t.Errorf("coil 7 = false, want true")
	}
}

func TestClientRejectsWrongTables(t *testing.T) {
	cl := &Client{}
	if _, err := cl.ReadRegisters(context.Background(), 1, TableCoil, 0, 1); err == nil {
		t.Errorf("ReadRegisters accepted a bit table")
	}
	if _, err := cl.ReadBits(context.Background(), 1, TableHolding, 0, 1); err == nil {
		t.Errorf("ReadBits accepted a register table")
	}
}
