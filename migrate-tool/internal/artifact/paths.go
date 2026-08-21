package artifact

import "path"

// 对象 key 是跨平台的逻辑路径，固定使用“/”；它不是当前宿主机的文件路径，
// 因此这里有意使用 path 而不是 filepath。

const (
	TypeMemfileHeader = "memfile-header"
	TypeRootfsHeader  = "rootfs-header"
	TypeMemfile       = "memfile"
	TypeRootfs        = "rootfs"
	TypeSnapfile      = "snapfile"
	TypeMetadata      = "metadata"
)

func MemfileHeader(buildID string) string { return path.Join(buildID, "memfile.header") }
func RootfsHeader(buildID string) string  { return path.Join(buildID, "rootfs.ext4.header") }
func Memfile(buildID string) string       { return path.Join(buildID, "memfile") }
func Rootfs(buildID string) string        { return path.Join(buildID, "rootfs.ext4") }
func Snapfile(buildID string) string      { return path.Join(buildID, "snapfile") }
func Metadata(buildID string) string      { return path.Join(buildID, "metadata.json") }
