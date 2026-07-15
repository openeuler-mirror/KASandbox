//go:build !mooncake
package storage
import(
    "context"
    "fmt"
    "time"
)

type mooncakeStorage struct {

}

var _ StorageProvider = (*mooncakeStorage)(nil)

func NewMooncakeStorageProvider(_ context.Context, bucketName string) (*mooncakeStorage, error) {
	return nil, nil
}

func (s *mooncakeStorage) DeleteObjectsWithPrefix(ctx context.Context, prefix string) error {
	return nil
}

func (s *mooncakeStorage) UploadSignedURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", fmt.Errorf("mooncake storage does not support signed URLs")
}

func (s *mooncakeStorage) OpenSeekable(_ context.Context, path string, _ SeekableObjectType) (Seekable, error) {
	return nil, fmt.Errorf("mooncake storage does not support seekable objects")
}

func (s *mooncakeStorage) OpenBlob(_ context.Context, path string, _ ObjectType) (Blob, error) {
	return nil, fmt.Errorf("mooncake storage does not support blobs")
}

func (s *mooncakeStorage) GetDetails() string {
	return "[Mooncake storage stub]"
}
