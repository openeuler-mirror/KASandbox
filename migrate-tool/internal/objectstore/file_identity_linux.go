//go:build linux

package objectstore

import (
	"fmt"
	"os"
	"syscall"
)

func localFileVersion(stat os.FileInfo) string {
	raw, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return metadataFileVersion(stat)
	}
	// inode/device 能识别同路径替换，ctime 能识别保留 mtime 的原地改写。
	return fmt.Sprintf(
		"file-%x-%x-%x-%x-%x-%x-%x",
		raw.Dev,
		raw.Ino,
		stat.Size(),
		stat.ModTime().UTC().UnixNano(),
		raw.Ctim.Sec,
		raw.Ctim.Nsec,
		raw.Nlink,
	)
}
