//go:build !linux

package numa

import (
	"fmt"
	"os"
)

func Initialize() error {
	if value := os.Getenv("NUMA_LOCALITY"); value != "" && value != "off" {
		return fmt.Errorf("NUMA_LOCALITY requires Linux")
	}
	return nil
}
