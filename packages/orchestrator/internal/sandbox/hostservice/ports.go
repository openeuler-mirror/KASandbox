package hostservice

// Ports collects the host-side ports allocated for the Android emulator
// services attached to a single sandbox. All values are loopback TCP/UDP
// ports on the orchestrator host. A zero value means the corresponding
// service did not allocate a port (e.g. config_server, which is vsock-only).
//
// The Sandbox propagates these values up to the API via the gRPC
// SandboxCreateResponse so external clients (adb, browser, ...) can reach
// the per-sandbox host services.
type Ports struct {
	AdbPort             int
	ModemSimulatorPort  int
	WebrtcHttpPort      int
	WebrtcStreamingPort int
}
