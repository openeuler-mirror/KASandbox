package api

import "github.com/e2b-dev/infra/packages/shared/pkg/apierrors"

type APIError = apierrors.APIError

type InvalidRequestError struct {
	Err error
}

func (e *InvalidRequestError) Error() string {
	return e.Err.Error()
}

func (e *InvalidRequestError) Unwrap() error {
	return e.Err
}
