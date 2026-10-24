package engine

import "testing"

func TestBuiltinDescriptors(t *testing.T) {
	cases := []struct {
		name       string
		desc       BackendDescriptor
		handler    string
		imageRoute string
	}{
		{"container", ContainerDescriptor, "container", ""},
		{"e2b", E2BDescriptor, "e2b", "e2b.dev/"},
		{"android", AndroidDescriptor, "android", "android.dev/"},
	}
	for _, tc := range cases {
		if tc.desc.Handler != tc.handler {
			t.Fatalf("%s handler = %q, want %q", tc.name, tc.desc.Handler, tc.handler)
		}
		if tc.imageRoute == "" {
			if len(tc.desc.ImagePrefixes) != 0 {
				t.Fatalf("%s image prefixes = %v, want none", tc.name, tc.desc.ImagePrefixes)
			}
			continue
		}
		if len(tc.desc.ImagePrefixes) != 1 || tc.desc.ImagePrefixes[0] != tc.imageRoute {
			t.Fatalf("%s image prefixes = %v, want [%s]", tc.name, tc.desc.ImagePrefixes, tc.imageRoute)
		}
	}
}
