package project

// drivers: — several field drivers on one scan. The loader's contract:
// driver: and drivers: together is an error; one entry is pure sugar for
// driver:; two or more wrap in io.Multi, where a tag owned by two drivers
// refuses to load naming both; and each child keeps its own status row.

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	nio "github.com/joyautomation/nautilus/io"
)

// multiBase is a manifest with the shared driver: block stripped, ready for
// a drivers: section to be appended.
func multiBase() string {
	return strings.Replace(manifest, "driver:\n  type: memory\n", "", 1)
}

// hostDriverYAML is one sparkplug-host entry for a drivers: list — the only
// current driver that builds fully offline, which is what makes multi-driver
// projects checkable in CI with no hardware anywhere.
func hostDriverYAML(indent, hostID, group, manifestFile string) string {
	i := indent
	return i + "- type: sparkplug-host\n" +
		i + "  broker: tcp://mqtt.invalid:1883\n" +
		i + "  host-id: " + hostID + "\n" +
		i + "  group-id: " + group + "\n" +
		i + "  manifest: " + manifestFile + "\n"
}

func hostManifestYAML(group, node, tag string) string {
	return "group: " + group + "\nnodes:\n    - edgenode: " + node + "\ntags:\n" +
		"    - { name: " + tag + ", node: " + node + ", device: \"\", metric: M, type: Double, arraylen: 0, writable: false }\n"
}

func TestDriverAndDriversBothSetIsAnError(t *testing.T) {
	files := fsys(manifest + "drivers:\n" + hostDriverYAML("  ", "h", "G", "m.yaml"))
	files["m.yaml"] = &fstest.MapFile{Data: []byte("group: G\n")}
	_, err := Load(files, "")
	if err == nil || !strings.Contains(err.Error(), "driver: and drivers: are both set") {
		t.Fatalf("err = %v, want the both-set teaching error", err)
	}
}

func TestDriversOneEntryIsSugarForDriver(t *testing.T) {
	files := fsys(multiBase() + "drivers:\n  - type: memory\n")
	p, err := Load(files, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Runtime.Driver.(*nio.Memory); !ok {
		t.Fatalf("driver = %T, want *io.Memory (no Multi wrapper for one entry)", p.Runtime.Driver)
	}
	if fn := p.DriverStatus(nil); fn != nil {
		t.Fatalf("a lone memory driver reports no status, got %+v", fn())
	}
}

func TestDriversBuildAMultiWithStatusRows(t *testing.T) {
	files := fsys(multiBase() + "drivers:\n" +
		hostDriverYAML("  ", "central-a", "GA", "a.yaml") +
		hostDriverYAML("  ", "central-b", "GB", "b.yaml"))
	files["a.yaml"] = &fstest.MapFile{Data: []byte(hostManifestYAML("GA", "W1", "W1_Level"))}
	files["b.yaml"] = &fstest.MapFile{Data: []byte(hostManifestYAML("GB", "W9", "W9_Level"))}
	p, err := Load(files, "")
	if err != nil {
		t.Fatal(err)
	}
	multi, ok := p.Runtime.Driver.(*nio.Multi)
	if !ok {
		t.Fatalf("driver = %T, want *io.Multi", p.Runtime.Driver)
	}

	// Names default to the type, deduped.
	var names []string
	for _, c := range multi.Children() {
		names = append(names, c.Name)
	}
	if want := []string{"sparkplug-host", "sparkplug-host-2"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("child names = %v, want %v", names, want)
	}

	// The merged input set spans both children (bindings + companions).
	ins := multi.InputNames()
	for _, want := range []string{"W1_Level", "W9_Level", "W1__Online", "W9__Online"} {
		if !contains(ins, want) {
			t.Errorf("merged InputNames %v is missing %q", ins, want)
		}
	}

	// One status row per child, not a blend.
	fn := p.DriverStatus(nil)
	if fn == nil {
		t.Fatal("DriverStatus must report a multi-driver set")
	}
	rows := fn()
	if len(rows) != 2 {
		t.Fatalf("status rows = %d, want one per child: %+v", len(rows), rows)
	}
	if rows[0].Kind != "sparkplug-host" || rows[1].Kind != "sparkplug-host" {
		t.Fatalf("row kinds: %q %q", rows[0].Kind, rows[1].Kind)
	}
	if rows[0].Name != "central-a" || rows[1].Name != "central-b" {
		t.Fatalf("row names: %q %q", rows[0].Name, rows[1].Name)
	}

	// And the Sparkplug device: health gate is ALL children healthy — with
	// neither broker connected it must read unhealthy, never nil (nil would
	// birth the device unconditionally).
	health := driverHealth(p.Runtime.Driver)
	if health == nil {
		t.Fatal("driverHealth(multi of hosts) = nil, want a real gate")
	}
	if health() {
		t.Fatal("both brokers are unreachable; the device must not read healthy")
	}
	if h := driverHealth(nio.NewMemory()); h != nil {
		t.Fatal("memory has no connection to be down; its health gate must stay nil")
	}
}

func TestDriversDuplicateTagNamesBothDrivers(t *testing.T) {
	files := fsys(multiBase() + "drivers:\n" +
		hostDriverYAML("  ", "central-a", "GA", "a.yaml") +
		hostDriverYAML("  ", "central-b", "GB", "b.yaml"))
	files["a.yaml"] = &fstest.MapFile{Data: []byte(hostManifestYAML("GA", "W1", "Shared_Level"))}
	files["b.yaml"] = &fstest.MapFile{Data: []byte(hostManifestYAML("GB", "W9", "Shared_Level"))}
	_, err := Load(files, "")
	if err == nil {
		t.Fatal("one tag owned by two drivers must refuse to load")
	}
	for _, want := range []string{"Shared_Level", "sparkplug-host", "sparkplug-host-2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestDriversMemoryCannotShareAScan(t *testing.T) {
	files := fsys(multiBase() + "drivers:\n" +
		"  - type: memory\n" +
		hostDriverYAML("  ", "h", "G", "a.yaml"))
	files["a.yaml"] = &fstest.MapFile{Data: []byte(hostManifestYAML("G", "W1", "W1_Level"))}
	_, err := Load(files, "")
	if err == nil || !strings.Contains(err.Error(), "InputNames") {
		t.Fatalf("err = %v, want the unroutable-driver teaching error", err)
	}
}

func TestDriversExplicitNameCollisionIsAnError(t *testing.T) {
	files := fsys(multiBase() + "drivers:\n" +
		hostDriverYAML("  ", "ha", "GA", "a.yaml") + "    name: field\n" +
		hostDriverYAML("  ", "hb", "GB", "b.yaml") + "    name: field\n")
	files["a.yaml"] = &fstest.MapFile{Data: []byte(hostManifestYAML("GA", "W1", "W1_Level"))}
	files["b.yaml"] = &fstest.MapFile{Data: []byte(hostManifestYAML("GB", "W9", "W9_Level"))}
	_, err := Load(files, "")
	if err == nil || !strings.Contains(err.Error(), `named "field"`) {
		t.Fatalf("err = %v, want the duplicate-name error", err)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
