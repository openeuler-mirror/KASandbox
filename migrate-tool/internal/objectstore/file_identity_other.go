//go:build !linux

package objectstore

import "os"

func localFileVersion(stat os.FileInfo) string {
	return metadataFileVersion(stat)
}
