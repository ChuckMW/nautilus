package codegen

// The broker path reads a metric's documentation from its birth properties:
// `documentation` becomes the binding's desc and `engUnit` its unit, a
// Template instance's members carry their own units, and both ride into the
// generated tag file. A birth that states none leaves both empty.

import (
	"strings"
	"testing"

	"github.com/joyautomation/nautilus/sparkplug"
	"github.com/joyautomation/nautilus/sparkplug/host"
	"github.com/joyautomation/nautilus/sparkplug/spb"
)

func TestBirthPropertiesCarryDescAndUnit(t *testing.T) {
	births := []host.Birth{{Group: "G", EdgeNode: "W6", Payload: wire(t, sparkplug.Payload{
		Metrics: []sparkplug.Metric{
			{Name: "bdSeq", Datatype: spb.DataType_Int64, Value: int64(0)},
			{Name: "Motor", Datatype: spb.DataType_Template, Value: &sparkplug.Template{
				IsDefinition: true,
				Metrics: []sparkplug.Metric{
					{Name: "Speed", Datatype: spb.DataType_Double, IsNull: true},
					{Name: "START", Datatype: spb.DataType_Boolean, IsNull: true},
				},
			}},
			{Name: "Well/Level", Datatype: spb.DataType_Double, Value: 1.0, Properties: []sparkplug.Property{
				{Key: sparkplug.PropEngUnit, Value: "ft"},
				{Key: sparkplug.PropDocumentation, Value: "Well 6 level"},
			}},
			{Name: "Motor1", Datatype: spb.DataType_Template, Properties: []sparkplug.Property{
				{Key: sparkplug.PropDocumentation, Value: "Transfer pump"},
			}, Value: &sparkplug.Template{
				TemplateRef: "Motor",
				Metrics: []sparkplug.Metric{
					{Name: "Speed", Datatype: spb.DataType_Double, Value: 0.0, Properties: []sparkplug.Property{
						{Key: sparkplug.PropEngUnit, Value: "rpm"},
					}},
					{Name: "START", Datatype: spb.DataType_Boolean, Value: false},
				},
			}},
			{Name: "Quiet", Datatype: spb.DataType_Double, Value: 2.0},
		},
	})}}
	m, err := FromBirths(births, Options{Writable: []string{"Motor1.Speed", "Motor1.START"}})
	if err != nil {
		t.Fatalf("FromBirths: %v", err)
	}
	level := bindingNamed(t, m, "W6_Well_Level")
	if level.Desc != "Well 6 level" || level.Unit != "ft" {
		t.Errorf("W6_Well_Level desc=%q unit=%q, want Well 6 level / ft", level.Desc, level.Unit)
	}
	motor := bindingNamed(t, m, "W6_Motor1")
	if motor.Desc != "Transfer pump" || motor.Unit != "" {
		t.Errorf("W6_Motor1 desc=%q unit=%q, want Transfer pump / none", motor.Desc, motor.Unit)
	}
	speed := bindingNamed(t, m, "W6_Motor1_Speed")
	if speed.Desc != "Transfer pump" || speed.Unit != "rpm" {
		t.Errorf("member W6_Motor1_Speed desc=%q unit=%q, want the metric's desc and its own rpm", speed.Desc, speed.Unit)
	}
	start := bindingNamed(t, m, "W6_Motor1_START")
	if start.Unit != "" {
		t.Errorf("member W6_Motor1_START unit=%q, want none (nothing documents it)", start.Unit)
	}
	quiet := bindingNamed(t, m, "W6_Quiet")
	if quiet.Desc != "" || quiet.Unit != "" {
		t.Errorf("an undocumented metric invented desc=%q unit=%q", quiet.Desc, quiet.Unit)
	}

	out, err := TagsYAML(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`- { name: W6_Well_Level, role: input, unit: "ft", desc: "Well 6 level" }`,
		`- { name: W6_Motor1_Speed, role: output, init: 0.0, unit: "rpm", desc: "Transfer pump" }`,
		`- { name: W6_Motor1_START, role: output, init: false, desc: "Transfer pump" }`,
		"- { name: W6_Quiet, role: input }\n",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("tag file missing %q:\n%s", want, out)
		}
	}
}
