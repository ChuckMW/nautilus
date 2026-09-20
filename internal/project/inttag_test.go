package project

// A manifest tag's init takes its kind from the program that declares it.
// YAML decodes `init: 0` as an int and the loader has always turned that
// into a float64, so a tag the logic declared `Counter : DINT` was seeded
// as a REAL and birthed to a Sparkplug host as a Double — then the first
// scan stored an integer. The type is in the compile (VAR_EXTERNAL), so the
// seed is made against it; nothing changes in the manifest.

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/joyautomation/nautilus/lang/ir"
	"github.com/joyautomation/nautilus/runtime"
	"github.com/joyautomation/nautilus/sparkplug"
	"github.com/joyautomation/nautilus/sparkplug/spb"
)

const intTagProgram = `PROGRAM Main
VAR_EXTERNAL
    Counter : DINT;
    TempSP  : REAL;
    Mode    : INT;
END_VAR
Counter := Counter + 1;
END_PROGRAM`

func intTagProject(tags string) fstest.MapFS {
	return fstest.MapFS{
		"nautilus.yaml": &fstest.MapFile{Data: []byte(
			"tasks:\n  - program: program.st\ntags:\n" + tags)},
		"program.st": &fstest.MapFile{Data: []byte(intTagProgram)},
	}
}

func seededValue(t *testing.T, tags, name string) ir.Value {
	t.Helper()
	proj, err := Load(intTagProject(tags), "")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := runtime.New(proj.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	v, err := rt.Tags().ReadGlobal(name)
	if err != nil {
		t.Fatalf("%s not seeded: %v", name, err)
	}
	return v
}

func birthDatatype(t *testing.T, name string, v ir.Value) spb.DataType {
	t.Helper()
	m, err := sparkplug.MetricFromValue(name, v, "")
	if err != nil {
		t.Fatal(err)
	}
	return m.Datatype
}

func TestIntegerTagSeedsAsTheProgramDeclaresIt(t *testing.T) {
	v := seededValue(t,
		"  - { name: Counter, role: state, init: 0 }\n"+
			"  - { name: TempSP,  role: setpoint, init: 65.0 }\n"+
			"  - { name: Mode,    role: setpoint, init: 2 }\n", "Counter")
	if v.Kind != ir.TypeInt || v.I != 0 {
		t.Fatalf("Counter seeded as %v (%+v), want an integer 0", v.Kind, v)
	}
	if dt := birthDatatype(t, "Counter", v); dt != spb.DataType_Int64 {
		t.Fatalf("Counter births as %v, want Int64", dt)
	}
}

// The other direction, which every existing example relies on: a REAL tag
// given an integer literal is still a REAL, and still births as a Double.
func TestRealTagWithIntegerLiteralStaysReal(t *testing.T) {
	v := seededValue(t,
		"  - { name: Counter, role: state, init: 0 }\n"+
			"  - { name: TempSP,  role: setpoint, init: 65 }\n"+
			"  - { name: Mode,    role: setpoint, init: 2 }\n", "TempSP")
	if v.Kind != ir.TypeReal || v.F != 65 {
		t.Fatalf("TempSP seeded as %v (%+v), want REAL 65", v.Kind, v)
	}
	if dt := birthDatatype(t, "TempSP", v); dt != spb.DataType_Double {
		t.Fatalf("TempSP births as %v, want Double", dt)
	}
}

// A tag no program declares has nothing to resolve against and keeps the
// behaviour it always had: a number seeds a REAL.
func TestUndeclaredTagWithIntegerLiteralSeedsReal(t *testing.T) {
	v := seededValue(t,
		"  - { name: Counter, role: state, init: 0 }\n"+
			"  - { name: TempSP,  role: setpoint, init: 65.0 }\n"+
			"  - { name: Mode,    role: setpoint, init: 2 }\n"+
			"  - { name: Spare,   role: state, init: 7 }\n", "Spare")
	if v.Kind != ir.TypeReal || v.F != 7 {
		t.Fatalf("Spare seeded as %v (%+v), want REAL 7", v.Kind, v)
	}
}

// An init the declared type cannot hold is a load error naming the tag and
// the type, not a silently rounded seed.
func TestFractionalInitOnIntegerTagIsAnError(t *testing.T) {
	proj, err := Load(intTagProject(
		"  - { name: Counter, role: state, init: 0 }\n"+
			"  - { name: TempSP,  role: setpoint, init: 65.0 }\n"+
			"  - { name: Mode,    role: setpoint, init: 2.5 }\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.New(proj.Runtime)
	if err == nil {
		t.Fatal("init: 2.5 on an INT tag built a runtime; want a load error")
	}
	for _, want := range []string{"tag Mode", "INT"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}
