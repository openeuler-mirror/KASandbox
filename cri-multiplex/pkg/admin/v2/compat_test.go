package adminv2

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoregistry"
)

func serviceMethods(desc grpc.ServiceDesc) []string {
	names := make([]string, 0, len(desc.Methods))
	for _, m := range desc.Methods {
		names = append(names, m.MethodName)
	}
	slices.Sort(names)
	return names
}

func TestV2RPCsMatchV1(t *testing.T) {
	lifecycle := []string{"Create", "Delete", "List", "ListCachedBuilds", "Update"}
	ops := []string{"CheckpointSandbox", "GetSandboxRuntime", "PauseSandbox"}
	if got := serviceMethods(SandboxService_ServiceDesc); !slices.Equal(got, lifecycle) {
		t.Fatalf("SandboxService methods = %v, want %v", got, lifecycle)
	}
	if got := serviceMethods(SandboxAdminService_ServiceDesc); !slices.Equal(got, ops) {
		t.Fatalf("SandboxAdminService methods = %v, want %v", got, ops)
	}
}

func TestV2ProtoDoesNotImportOrchestrator(t *testing.T) {
	fd, err := protoregistry.GlobalFiles.FindFileByPath("admin_v2.proto")
	if err != nil {
		t.Fatalf("admin_v2.proto not registered: %v", err)
	}
	for i := 0; i < fd.Imports().Len(); i++ {
		if strings.Contains(string(fd.Imports().Get(i).Path()), "orchestrator") {
			t.Fatalf("admin_v2.proto imports %s", fd.Imports().Get(i).Path())
		}
	}
}
