package fc

import (
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Once the holder has exited and been reaped, holderGone is set, so starts
// stop using /proc/<pid>/ns/mnt: the pid may already belong to another
// process.
func TestWatchHolderMarksHolderGone(t *testing.T) { //nolint:paralleltest // pinnedMountNS is process-wide
	pinnedMountNS.holderGone.Store(false)
	t.Cleanup(func() { pinnedMountNS.holderGone.Store(false) })

	cmd := exec.Command("true")
	require.NoError(t, cmd.Start())
	watchHolder(cmd)

	require.Eventually(t, pinnedMountNS.holderGone.Load, 5*time.Second, 10*time.Millisecond)
}
