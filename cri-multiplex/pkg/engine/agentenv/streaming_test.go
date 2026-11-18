package agentenv

import (
	"context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
	"net/url"
	"strings"
	"testing"
)

func TestPortForwardRegistersAuthorizedPorts(t *testing.T) {
	instance, _ := newReadyContainerTestEngine(t)
	defer instance.Close()
	pod, ok := instance.getPod("cri-1")
	if !ok {
		t.Fatal("pod projection not found")
	}
	pod.State = criStateReady
	if err := instance.putPod(pod); err != nil {
		t.Fatalf("putPod: %v", err)
	}

	response, err := instance.PortForward(context.Background(), &runtime.PortForwardRequest{
		PodSandboxId: "cri-1",
		Port:         []int32{8080, 8443},
	})
	if err != nil {
		t.Fatalf("PortForward: %v", err)
	}
	parsed, err := url.Parse(response.GetUrl())
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	token := strings.TrimPrefix(parsed.Path, "/portforward/")
	request, ok := instance.getPortForwardRequest(token)
	if !ok {
		t.Fatal("port-forward request not registered")
	}
	if request.podIP == "" || len(request.ports) != 2 {
		t.Fatalf("unexpected port-forward request: %+v", request)
	}
	if _, ok := request.ports[8080]; !ok {
		t.Fatalf("authorized ports = %#v", request.ports)
	}
}

func TestPortForwardRejectsInvalidPorts(t *testing.T) {
	instance, _ := newReadyContainerTestEngine(t)
	defer instance.Close()
	for _, ports := range [][]int32{{0}, {65536}} {
		_, err := instance.PortForward(context.Background(), &runtime.PortForwardRequest{
			PodSandboxId: "cri-1",
			Port:         ports,
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("ports=%v code=%v, want InvalidArgument", ports, status.Code(err))
		}
	}
}
