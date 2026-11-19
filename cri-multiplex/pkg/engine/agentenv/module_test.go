package agentenv

import "testing"

func TestDescriptorRoutesVirtualImages(t *testing.T) {
	if len(Descriptor.ImagePrefixes) != 1 || Descriptor.ImagePrefixes[0] != VirtualImagePrefix {
		t.Fatalf("image prefixes = %v, want %q", Descriptor.ImagePrefixes, VirtualImagePrefix)
	}
}
