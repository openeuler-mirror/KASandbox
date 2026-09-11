package artifact

import "testing"

func TestMetadataOS(t *testing.T) {
	for _, tt := range []struct{ raw, want string }{
		{`{"template":{"build_id":"build"}}`, OSLinux},
		{`{"template":{"build_id":"build","os_type":"linux"}}`, OSLinux},
		{`{"template":{"build_id":"build","os_type":"android","vmm_type":"stratovirt","unknown":"retained"}}`, OSAndroid},
		{`{"template":{"build_id":"other","os_type":"android"}}`, ""},
		{`{"template":{"build_id":"build","os_type":"windows"}}`, ""},
		{`{"template":{"build_id":"build","os_type":42}}`, ""},
		{`{}`, ""},
		{`{`, ""},
	} {
		got, err := MetadataOS([]byte(tt.raw), "build")
		if tt.want == "" {
			if err == nil {
				t.Errorf("accepted %s", tt.raw)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("MetadataOS(%s) = %s, %v", tt.raw, got, err)
		}
	}
}
