package bundle

import (
	"testing"

	"gitcode.com/openeuler/KASandbox/migrate-tool/internal/artifact"
)

func TestRejectsInvalidBuildLayouts(t *testing.T) {
	valid := func() Manifest {
		return Manifest{FormatVersion: 2, BuildLayouts: []BuildLayout{{BuildID: "build", OSType: "android", Disks: artifact.DiskNames("android")}}}
	}
	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"missing layout", func(m *Manifest) { m.BuildLayouts = nil }},
		{"extra layout", func(m *Manifest) { m.BuildLayouts = append(m.BuildLayouts, m.BuildLayouts[0]) }},
		{"unknown build", func(m *Manifest) { m.BuildLayouts[0].BuildID = "other" }},
		{"missing disk", func(m *Manifest) { m.BuildLayouts[0].Disks = []string{"rootfs.ext4", "persistent.img"} }},
		{"duplicate disk", func(m *Manifest) {
			m.BuildLayouts[0].Disks = []string{"rootfs.ext4", "persistent.img", "persistent.img"}
		}},
		{"wrong order", func(m *Manifest) { m.BuildLayouts[0].Disks = []string{"sdcard.img", "persistent.img", "rootfs.ext4"} }},
		{"unknown OS", func(m *Manifest) { m.BuildLayouts[0].OSType = "windows" }},
		{"legacy with layout", func(m *Manifest) { m.FormatVersion = 1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := valid()
			tt.mutate(&m)
			if _, err := validateLayouts(m, Records{Builds: []BuildRecord{{ID: "build"}}}); err == nil {
				t.Fatal("invalid layout accepted")
			}
		})
	}
	if _, err := validateLayouts(valid(), Records{Builds: []BuildRecord{{ID: "build"}}}); err != nil {
		t.Fatal(err)
	}
}
