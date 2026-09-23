package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/e2b-dev/infra/packages/shared/pkg/env"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

const defaultSignedUploadMaxBytes = 10 << 30 // 10 GiB

// SignedUploadHandler handles uploads for provider-minted signed URLs (currently
// Mooncake), which is why the CLI talks to the orchestrator directly. It is only
// usable when buildStorage implements SignedUploadVerifier.
func (s *ServerStore) SignedUploadHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}

		verifier, ok := s.buildStorage.(storage.SignedUploadVerifier)
		if !ok {
			http.Error(w, "signed uploads not supported", http.StatusNotFound)

			return
		}

		q := r.URL.Query()
		path := q.Get("path")
		expires, err := strconv.ParseInt(q.Get("expires"), 10, 64)
		if path == "" || err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)

			return
		}
		if err := verifier.VerifySignedUpload(path, expires, q.Get("sig")); err != nil {
			http.Error(w, "forbidden: "+err.Error(), http.StatusForbidden)

			return
		}

		blob, err := s.buildStorage.OpenBlob(r.Context(), path, storage.BuildLayerFileObjectType)
		if err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)

			return
		}
		storer, ok := blob.(storage.StreamStorer)
		if !ok {
			http.Error(w, "streaming store not supported", http.StatusInternalServerError)

			return
		}

		maxBytes := defaultSignedUploadMaxBytes
		if v, err := env.GetEnvAsInt("MOONCAKE_UPLOAD_MAX_BYTES", defaultSignedUploadMaxBytes); err == nil && v > 0 {
			maxBytes = v
		}
		body := http.MaxBytesReader(w, r.Body, int64(maxBytes))
		defer r.Body.Close()

		if err := storer.StoreReader(r.Context(), body); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)

				return
			}
			http.Error(w, "upload failed", http.StatusInternalServerError)

			return
		}

		w.WriteHeader(http.StatusOK)
	})
}
