// properties_test.go: a birth states each tag's documentation as Sparkplug
// metric properties — `unit:` as engUnit, `desc:` as documentation — so a
// host discovers the whole tag database from the birth certificate, units
// and descriptions included, instead of someone retyping them. Data
// messages carry none, and the decoder brings them back.

package sparkplug

import (
	"testing"
	"time"

	nio "github.com/joyautomation/nautilus/io"
	"github.com/joyautomation/nautilus/lang/ir"
	"github.com/joyautomation/nautilus/runtime"
	"github.com/joyautomation/nautilus/sparkplug/spb"
)

func TestPropertiesRoundTrip(t *testing.T) {
	in := Metric{Name: "Level", Datatype: spb.DataType_Double, Value: 1.5, Properties: []Property{
		{PropEngUnit, "ft"},
		{PropDocumentation, "Wet well level"},
		{"engHigh", 12.0},
		{"count", int64(-3)},
		{"flag", true},
	}}
	b, err := (Payload{Metrics: []Metric{in}}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodePayload(b)
	if err != nil {
		t.Fatal(err)
	}
	got := out.Metrics[0].Properties
	if len(got) != len(in.Properties) {
		t.Fatalf("decoded %d properties, want %d: %+v", len(got), len(in.Properties), got)
	}
	for i, p := range in.Properties {
		if got[i] != p {
			t.Errorf("property %d = %+v, want %+v", i, got[i], p)
		}
	}
	if u := out.Metrics[0].PropertyString(PropEngUnit); u != "ft" {
		t.Errorf("PropertyString(engUnit) = %q", u)
	}
}

const propsTypesST = `
TYPE
	Motor : STRUCT
		Speed : REAL;
		Run   : BOOL;
	END_STRUCT;
END_TYPE
`

const propsProgramST = `
PROGRAM Main
VAR_EXTERNAL
	Level  : REAL;
	Motor1 : Motor;
END_VAR
END_PROGRAM
`

// A born node's NBIRTH documents Level (unit + desc) and Motor1's Speed
// member (unit, via the dotted tag-meta key); the NDATA that follows a
// change carries no properties at all.
func TestBirthStatesUnitAndDescAsProperties(t *testing.T) {
	rt, err := runtime.New(runtime.Options{
		Program:   propsProgramST,
		Libraries: []string{propsTypesST},
		Driver:    nio.NewMemory(),
		Scan:      50 * time.Millisecond,
		Tags: []runtime.TagDef{
			runtime.State("Level", 3.5, runtime.Unit("ft"), runtime.Desc("Wet well level")),
			runtime.Typed("Motor1", runtime.RoleState, "Motor", runtime.Desc("Pump 1 motor")),
		},
		Meta: map[string]runtime.TagMeta{"Motor1.Speed": {Unit: "rpm"}},
	})
	if err != nil {
		t.Fatalf("build runtime: %v", err)
	}
	n, err := New(rt, Config{GroupID: "g", EdgeNode: "e"})
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeClient{open: true}
	n.cli = fc
	if err := n.birth(); err != nil {
		t.Fatal(err)
	}
	births := fc.published("NBIRTH")
	if len(births) != 1 {
		t.Fatalf("%d NBIRTH, want 1", len(births))
	}
	p, err := DecodePayload(births[0].payload)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Metric{}
	for _, m := range p.Metrics {
		byName[m.Name] = m
	}

	level := byName["Level"]
	if u, d := level.PropertyString(PropEngUnit), level.PropertyString(PropDocumentation); u != "ft" || d != "Wet well level" {
		t.Errorf("Level: engUnit=%q documentation=%q, want ft / Wet well level (%+v)", u, d, level.Properties)
	}
	motor := byName["Motor1"]
	if d := motor.PropertyString(PropDocumentation); d != "Pump 1 motor" {
		t.Errorf("Motor1: documentation=%q, want Pump 1 motor", d)
	}
	tmpl, ok := motor.Value.(*Template)
	if !ok || tmpl == nil {
		t.Fatalf("Motor1 is not a template instance: %+v", motor)
	}
	var speed, run Metric
	for _, m := range tmpl.Metrics {
		switch m.Name {
		case "Speed":
			speed = m
		case "Run":
			run = m
		}
	}
	if u := speed.PropertyString(PropEngUnit); u != "rpm" {
		t.Errorf("Motor1.Speed: engUnit=%q, want rpm (%+v)", u, speed.Properties)
	}
	if len(run.Properties) != 0 {
		t.Errorf("Motor1.Run has properties %+v, want none (nothing documents it)", run.Properties)
	}
	// The protocol metrics and the Motor definition carry none either.
	for _, name := range []string{"bdSeq", "Node Control/Rebirth", "Motor"} {
		if m, ok := byName[name]; ok && len(m.Properties) != 0 {
			t.Errorf("%s carries properties %+v", name, m.Properties)
		}
	}

	// Data: no properties.
	rt.Tags().Set("Level", ir.RealVal(4.0))
	tickWithin(t, n, 2*time.Second)
	data := fc.published("NDATA")
	if len(data) != 1 {
		t.Fatalf("%d NDATA after a change, want 1", len(data))
	}
	d, err := DecodePayload(data[0].payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range d.Metrics {
		if len(m.Properties) != 0 {
			t.Errorf("NDATA metric %s carries properties %+v; births only", m.Name, m.Properties)
		}
	}
}
