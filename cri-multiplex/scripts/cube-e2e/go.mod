module github.com/cri-multiplex/scripts/cube-e2e

go 1.25.4

require (
	github.com/cri-multiplex v0.0.0
	google.golang.org/grpc v1.80.0
	k8s.io/cri-api v0.34.1
)

require (
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/cri-multiplex => ../../
