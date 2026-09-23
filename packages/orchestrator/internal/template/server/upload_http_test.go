package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

var (
	errTestBadSignature = errors.New("bad signature")
	errTestExpired      = errors.New("expired")
	errTestStore        = errors.New("store failed")
)

// baseUploadProvider implements StorageProvider without the optional
// SignedUploadVerifier interface.
type baseUploadProvider struct {
	stored   []byte
	storeErr error
}

var _ storage.StorageProvider = (*baseUploadProvider)(nil)

func (f *baseUploadProvider) DeleteObjectsWithPrefix(context.Context, string) error { return nil }

func (f *baseUploadProvider) UploadSignedURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

func (f *baseUploadProvider) OpenBlob(context.Context, string, storage.ObjectType) (storage.Blob, error) {
	return &fakeBlob{provider: f}, nil
}

func (f *baseUploadProvider) OpenSeekable(context.Context, string, storage.SeekableObjectType) (storage.Seekable, error) {
	return nil, errors.New("not supported")
}

func (f *baseUploadProvider) GetDetails() string { return "[fake]" }

// signableUploadProvider additionally implements SignedUploadVerifier.
type signableUploadProvider struct {
	baseUploadProvider

	verifyCalls int
}

var _ storage.SignedUploadVerifier = (*signableUploadProvider)(nil)

func (f *signableUploadProvider) VerifySignedUpload(path string, expires int64, sig string) error {
	f.verifyCalls++

	if sig != "good" {
		return errTestBadSignature
	}
	if path == "" {
		return errTestBadSignature
	}
	if time.Now().Unix() > expires {
		return errTestExpired
	}

	return nil
}

type fakeBlob struct {
	provider *baseUploadProvider
}

var _ storage.Blob = (*fakeBlob)(nil)
var _ storage.StreamStorer = (*fakeBlob)(nil)

func (b *fakeBlob) WriteTo(context.Context, io.Writer) (int64, error) { return 0, nil }

func (b *fakeBlob) Put(context.Context, []byte) error { return nil }

func (b *fakeBlob) Exists(context.Context) (bool, error) { return false, nil }

func (b *fakeBlob) StoreReader(_ context.Context, r io.Reader) error {
	if b.provider.storeErr != nil {
		return b.provider.storeErr
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	b.provider.stored = data

	return nil
}

func newUploadRequest(t *testing.T, method string, body io.Reader, query url.Values) *http.Request {
	t.Helper()

	target := storage.SignedUploadPath
	if query != nil {
		target += "?" + query.Encode()
	}

	return httptest.NewRequest(method, target, body)
}

func signedQuery(path string, expires int64, sig string) url.Values {
	q := url.Values{}
	q.Set("path", path)
	q.Set("expires", fmt.Sprintf("%d", expires))
	q.Set("sig", sig)

	return q
}

func TestSignedUploadHandler_Success(t *testing.T) {
	t.Parallel()

	provider := &signableUploadProvider{}
	store := &ServerStore{buildStorage: provider}

	body := "layer tar bytes"
	req := newUploadRequest(t, http.MethodPut, strings.NewReader(body), signedQuery("team/files/abc.tar", time.Now().Add(time.Minute).Unix(), "good"))
	rec := httptest.NewRecorder()

	store.SignedUploadHandler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, body, string(provider.stored))
	assert.Equal(t, 1, provider.verifyCalls)
}

func TestSignedUploadHandler_Rejects(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		method    string
		body      io.Reader
		query     url.Values
		storeErr  error
		expected  int
		expectSig bool
	}{
		{
			name:      "method not allowed",
			method:    http.MethodPost,
			body:      strings.NewReader("body"),
			query:     signedQuery("team/files/abc.tar", time.Now().Add(time.Minute).Unix(), "good"),
			expected:  http.StatusMethodNotAllowed,
			expectSig: false,
		},
		{
			name:      "missing path",
			method:    http.MethodPut,
			body:      strings.NewReader("body"),
			query:     url.Values{"expires": []string{"1"}, "sig": []string{"good"}},
			expected:  http.StatusBadRequest,
			expectSig: false,
		},
		{
			name:      "non numeric expires",
			method:    http.MethodPut,
			body:      strings.NewReader("body"),
			query:     url.Values{"path": []string{"team/files/abc.tar"}, "expires": []string{"later"}, "sig": []string{"good"}},
			expected:  http.StatusBadRequest,
			expectSig: false,
		},
		{
			name:      "invalid signature",
			method:    http.MethodPut,
			body:      strings.NewReader("body"),
			query:     signedQuery("team/files/abc.tar", time.Now().Add(time.Minute).Unix(), "bad"),
			expected:  http.StatusForbidden,
			expectSig: true,
		},
		{
			name:      "expired signature",
			method:    http.MethodPut,
			body:      strings.NewReader("body"),
			query:     signedQuery("team/files/abc.tar", time.Now().Add(-time.Minute).Unix(), "good"),
			expected:  http.StatusForbidden,
			expectSig: true,
		},
		{
			name:      "upload too large",
			method:    http.MethodPut,
			body:      strings.NewReader("body"),
			query:     signedQuery("team/files/abc.tar", time.Now().Add(time.Minute).Unix(), "good"),
			storeErr:  &http.MaxBytesError{Limit: 1},
			expected:  http.StatusRequestEntityTooLarge,
			expectSig: true,
		},
		{
			name:      "store failed",
			method:    http.MethodPut,
			body:      strings.NewReader("body"),
			query:     signedQuery("team/files/abc.tar", time.Now().Add(time.Minute).Unix(), "good"),
			storeErr:  errTestStore,
			expected:  http.StatusInternalServerError,
			expectSig: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			provider := &signableUploadProvider{baseUploadProvider: baseUploadProvider{storeErr: tc.storeErr}}
			store := &ServerStore{buildStorage: provider}

			req := newUploadRequest(t, tc.method, tc.body, tc.query)
			rec := httptest.NewRecorder()

			store.SignedUploadHandler().ServeHTTP(rec, req)

			assert.Equal(t, tc.expected, rec.Code)

			expectedSigCalls := 0
			if tc.expectSig {
				expectedSigCalls = 1
			}
			assert.Equal(t, expectedSigCalls, provider.verifyCalls)
		})
	}
}

func TestSignedUploadHandler_UnsupportedProvider(t *testing.T) {
	t.Parallel()

	store := &ServerStore{buildStorage: &baseUploadProvider{}}

	req := newUploadRequest(t, http.MethodPut, strings.NewReader("body"), signedQuery("team/files/abc.tar", time.Now().Add(time.Minute).Unix(), "good"))
	rec := httptest.NewRecorder()

	store.SignedUploadHandler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSignedUploadHandler_BodyRead(t *testing.T) {
	t.Parallel()

	// A body larger than a single read buffer must be stored in full.
	payload := strings.Repeat("e2b-layer-", 1024)
	provider := &signableUploadProvider{}
	store := &ServerStore{buildStorage: provider}

	req := newUploadRequest(t, http.MethodPut, strings.NewReader(payload), signedQuery("team/files/abc.tar", time.Now().Add(time.Minute).Unix(), "good"))
	rec := httptest.NewRecorder()

	store.SignedUploadHandler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, payload, string(provider.stored))
}
