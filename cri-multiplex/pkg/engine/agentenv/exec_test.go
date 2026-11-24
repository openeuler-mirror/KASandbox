package agentenv

import (
	"context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
	"testing"
)

// 即使 Runtime 未连接，CRI 命令入口也必须明确返回不支持。
func TestCRIExecutionUsesSDK(t *testing.T) {
	e := &Engine{}
	_, syncErr := e.ExecSync(context.Background(), &runtime.ExecSyncRequest{ContainerId: "c", Cmd: []string{"true"}})
	_, execErr := e.Exec(context.Background(), &runtime.ExecRequest{ContainerId: "c", Cmd: []string{"true"}})
	_, attachErr := e.Attach(context.Background(), &runtime.AttachRequest{ContainerId: "c"})
	_, portErr := e.PortForward(context.Background(), &runtime.PortForwardRequest{})
	for name, err := range map[string]error{"ExecSync": syncErr, "Exec": execErr, "Attach": attachErr, "PortForward": portErr} {
		if status.Code(err) != codes.Unimplemented {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
