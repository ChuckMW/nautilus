package modbus

import (
	"strings"
	"testing"
)

// mk builds a one-source manifest around the given bindings.
func mk(maxBlock int, tags ...TagBinding) Manifest {
	return Manifest{
		Sources: []Source{{ID: "A", Host: "h", MaxBlock: maxBlock}},
		Tags:    tags,
	}
}

func holding(name string, addr uint16, format string) TagBinding {
	return TagBinding{Name: name, Source: "A", Table: TableHolding, Address: addr, Format: format}
}

func mustPlan(t *testing.T, m Manifest, gap int) Plan {
	t.Helper()
	p, err := BuildPlan(m, gap)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return p
}

func TestPlanCoalescesAdjacent(t *testing.T) {
	p := mustPlan(t, mk(0,
		holding("T0", 0, "int16"),
		holding("T1", 1, "uint16"),
		holding("T2", 2, "float32"),
	), -1)
	if len(p.Blocks) != 1 {
		t.Fatalf("blocks = %d, want 1\n%s", len(p.Blocks), p)
	}
	b := p.Blocks[0]
	if b.Start != 0 || b.Count != 4 || len(b.Bindings) != 3 {
		t.Errorf("block = %+v", b)
	}
	if b.FC() != FCReadHoldingRegisters || b.Class != DefaultClass {
		t.Errorf("block fc/class = %d %q", b.FC(), b.Class)
	}
}

func TestPlanGapTolerance(t *testing.T) {
	// Default gap 8: a hole of 7 registers is bridged, reading unused words
	// instead of paying a second round-trip.
	p := mustPlan(t, mk(0, holding("T0", 0, "int16"), holding("T8", 8, "int16")), -1)
	if len(p.Blocks) != 1 || p.Blocks[0].Count != 9 {
		t.Fatalf("gap 8: %s", p)
	}
	// The hole just past the tolerance splits.
	p = mustPlan(t, mk(0, holding("T0", 0, "int16"), holding("T10", 10, "int16")), -1)
	if len(p.Blocks) != 2 {
		t.Fatalf("beyond gap: %s", p)
	}
	// Gap 0 is the escape hatch for devices that fault reads touching
	// unimplemented registers: only contiguous bindings merge.
	p = mustPlan(t, mk(0, holding("T0", 0, "int16"), holding("T1", 1, "int16"), holding("T3", 3, "int16")), 0)
	if len(p.Blocks) != 2 || p.Blocks[0].Count != 2 {
		t.Fatalf("gap 0: %s", p)
	}
}

func TestPlanRegisterCap(t *testing.T) {
	// 130 contiguous registers cannot fit one FC3 read (cap 125).
	var tags []TagBinding
	for i := range 130 {
		tags = append(tags, holding("T"+itoa(i), uint16(i), "uint16"))
	}
	p := mustPlan(t, mk(0, tags...), -1)
	if len(p.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2\n%s", len(p.Blocks), p)
	}
	if p.Blocks[0].Count != 125 || p.Blocks[1].Count != 5 {
		t.Errorf("counts = %d, %d", p.Blocks[0].Count, p.Blocks[1].Count)
	}
}

func TestPlanMaxBlockOverride(t *testing.T) {
	// An ADAM that caps reads at 10 registers splits where the protocol
	// would not.
	var tags []TagBinding
	for i := range 12 {
		tags = append(tags, holding("T"+itoa(i), uint16(i), "uint16"))
	}
	p := mustPlan(t, mk(10, tags...), -1)
	if len(p.Blocks) != 2 || p.Blocks[0].Count != 10 || p.Blocks[1].Count != 2 {
		t.Fatalf("maxblock: %s", p)
	}
}

func TestPlanMultiRegisterValueNeverSplits(t *testing.T) {
	// A float32 whose second word would cross the cap moves whole into the
	// next block.
	p := mustPlan(t, mk(4,
		holding("T0", 0, "int16"),
		holding("T1", 1, "int16"),
		holding("T2", 2, "int16"),
		holding("F", 3, "float32"), // words 3..4; 5 > cap 4
	), -1)
	if len(p.Blocks) != 2 {
		t.Fatalf("blocks = %d\n%s", len(p.Blocks), p)
	}
	if p.Blocks[1].Start != 3 || p.Blocks[1].Count != 2 {
		t.Errorf("second block = %+v", p.Blocks[1])
	}
	// A single value wider than the cap is impossible — loud error.
	if _, err := BuildPlan(mk(2, holding("D", 0, "float64")), -1); err == nil ||
		!strings.Contains(err.Error(), "float64 needs 4 registers") {
		t.Errorf("float64 vs maxblock 2: %v", err)
	}
}

func TestPlanCoilCap(t *testing.T) {
	var tags []TagBinding
	for i := range 2001 {
		tags = append(tags, TagBinding{Name: "C" + itoa(i), Source: "A", Table: TableCoil, Address: uint16(i)})
	}
	p := mustPlan(t, mk(0, tags...), -1)
	if len(p.Blocks) != 2 || p.Blocks[0].Count != 2000 || p.Blocks[0].FC() != FCReadCoils {
		t.Fatalf("coil cap: %d blocks, first %+v", len(p.Blocks), p.Blocks[0])
	}
}

func TestPlanMixedTablesAndClasses(t *testing.T) {
	m := mk(0,
		holding("H", 0, "int16"),
		TagBinding{Name: "I", Source: "A", Table: TableInput, Address: 0, Format: "int16"},
		TagBinding{Name: "C", Source: "A", Table: TableCoil, Address: 0},
		TagBinding{Name: "D", Source: "A", Table: TableDiscrete, Address: 0},
		TagBinding{Name: "Slow", Source: "A", Table: TableHolding, Address: 1, Format: "int16", ScanClass: "slow"},
	)
	p := mustPlan(t, m, -1)
	if len(p.Blocks) != 5 {
		t.Fatalf("blocks = %d, want 5 (tables and classes never merge)\n%s", len(p.Blocks), p)
	}
	// Deterministic order: holding before input before coil before discrete,
	// classes alphabetical within a table.
	got := make([]string, len(p.Blocks))
	for i, b := range p.Blocks {
		got[i] = b.Table + "/" + b.Class
	}
	want := []string{"holding/default", "holding/slow", "input/default", "coil/default", "discrete/default"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestPlanSkipsNoPollAndWriteOnly(t *testing.T) {
	m := mk(0,
		holding("Polled", 0, "int16"),
		TagBinding{Name: "Cataloged", Source: "A", Table: TableHolding, Address: 10, Format: "int16", ScanClass: NoPoll},
		TagBinding{Name: "Cmd", Source: "A", Table: TableHolding, Address: 20, Format: "int16", Writable: true, WriteOnly: true},
		TagBinding{Name: "Setpoint", Source: "A", Table: TableHolding, Address: 1, Format: "int16", Writable: true},
	)
	p := mustPlan(t, m, -1)
	if len(p.Blocks) != 1 {
		t.Fatalf("blocks:\n%s", p)
	}
	names := map[string]bool{}
	for _, b := range p.Blocks[0].Bindings {
		names[b.Name] = true
	}
	// The writable-but-readable Setpoint IS polled (read-back is the source
	// of truth after a restart); NoPoll and WriteOnly are not.
	if !names["Polled"] || !names["Setpoint"] || names["Cataloged"] || names["Cmd"] {
		t.Errorf("polled bindings = %v", names)
	}
}

func TestPlanSharedAddressBindings(t *testing.T) {
	p := mustPlan(t, mk(0,
		holding("Status", 5, "uint16"),
		TagBinding{Name: "Run", Source: "A", Table: TableHolding, Address: 5, Format: "bit:0"},
		TagBinding{Name: "Fault", Source: "A", Table: TableHolding, Address: 5, Format: "bit:1"},
	), -1)
	if len(p.Blocks) != 1 || p.Blocks[0].Count != 1 || len(p.Blocks[0].Bindings) != 3 {
		t.Fatalf("shared word: %s", p)
	}
}

func TestPlanPerSourceGrouping(t *testing.T) {
	m := Manifest{
		Sources: []Source{{ID: "A", Host: "h"}, {ID: "B", Host: "h2"}},
		Tags: []TagBinding{
			{Name: "A0", Source: "A", Table: TableHolding, Address: 0, Format: "int16"},
			{Name: "B0", Source: "B", Table: TableHolding, Address: 0, Format: "int16"},
			{Name: "B1", Source: "B", Table: TableHolding, Address: 1, Format: "int16"},
		},
	}
	p := mustPlan(t, m, -1)
	if len(p.Blocks) != 2 || p.Blocks[0].Source != "A" || p.Blocks[1].Source != "B" {
		t.Fatalf("per-source: %s", p)
	}
}

func TestPlanValidatesFirst(t *testing.T) {
	m := mk(0, TagBinding{Name: "T", Source: "Z", Table: TableHolding, Address: 0, Format: "int16"})
	if _, err := BuildPlan(m, -1); err == nil || !strings.Contains(err.Error(), `unknown source "Z"`) {
		t.Fatalf("BuildPlan should validate: %v", err)
	}
}

func TestPlanString(t *testing.T) {
	p := mustPlan(t, mk(0,
		holding("FTIR_I_CO", 14, "float32"),
		holding("FTIR_I_NO", 16, "float32"),
	), -1)
	s := p.String()
	for _, want := range []string{"source A: 1 block", "FC3 holding 14..17 (4 registers, class default)", "FTIR_I_CO@14", "FTIR_I_NO@16"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() missing %q:\n%s", want, s)
		}
	}
	if empty := (Plan{}).String(); !strings.Contains(empty, "no blocks") {
		t.Errorf("empty plan: %q", empty)
	}
}

// itoa avoids strconv in half the call sites' imports; test-local.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
