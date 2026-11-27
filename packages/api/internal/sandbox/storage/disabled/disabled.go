// Package disabled provides the sandbox storage, reservations and routing
// catalog the api uses when Redis is not configured.
//
// Sandbox state lives only in Redis, so without it the api does not track
// sandboxes: reads find nothing and writes fail with ErrRedisDisabled.
// Templates are unaffected, they come from the database. This suits a single
// api instance whose sandboxes are scheduled outside the api, for example by
// Flux / openFuyao through cri-multiplex, which talks to the orchestrators
// directly.
package disabled

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/api/internal/sandbox/sandboxtypes"
	catalog "github.com/e2b-dev/infra/packages/shared/pkg/sandbox-catalog"
)

// ErrRedisDisabled is returned for sandbox operations that need Redis.
var ErrRedisDisabled = errors.New("sandbox operations need Redis, which is not configured for this api")

// errNotTracked is what lookups of a single sandbox return: the sandbox is not
// found, and the reason is that nothing is tracked.
var errNotTracked = fmt.Errorf("%w (%w)", sandboxtypes.ErrNotFound, ErrRedisDisabled)

// Storage tracks no sandboxes.
type Storage struct{}

var _ sandboxtypes.Storage = Storage{}

func (Storage) Add(context.Context, sandboxtypes.Sandbox) error { return ErrRedisDisabled }

func (Storage) Get(context.Context, uuid.UUID, string) (sandboxtypes.Sandbox, error) {
	return sandboxtypes.Sandbox{}, errNotTracked
}

func (Storage) Remove(context.Context, uuid.UUID, string) error { return errNotTracked }

func (Storage) TeamItems(context.Context, uuid.UUID, []sandboxtypes.State) ([]sandboxtypes.Sandbox, error) {
	return nil, nil
}

func (Storage) ExpiredItems(context.Context) ([]sandboxtypes.Sandbox, error) { return nil, nil }

func (Storage) TeamsWithSandboxCount(context.Context) (map[uuid.UUID]int64, error) {
	return map[uuid.UUID]int64{}, nil
}

func (Storage) Update(context.Context, uuid.UUID, string, func(sandboxtypes.Sandbox) (sandboxtypes.Sandbox, error)) (sandboxtypes.Sandbox, error) {
	return sandboxtypes.Sandbox{}, errNotTracked
}

func (Storage) StartRemoving(context.Context, uuid.UUID, string, sandboxtypes.RemoveOpts) (sandboxtypes.Sandbox, bool, func(context.Context, error), error) {
	return sandboxtypes.Sandbox{}, false, nil, errNotTracked
}

func (Storage) RestoreRunning(context.Context, uuid.UUID, string, sandboxtypes.State, time.Duration) (sandboxtypes.Sandbox, error) {
	return sandboxtypes.Sandbox{}, errNotTracked
}

func (Storage) WaitForStateChange(context.Context, uuid.UUID, string) error { return errNotTracked }

// Reconcile reports no orphans. Every sandbox on a node is missing from an
// empty store, so treating them as orphans would kill the sandboxes scheduled
// outside the api.
func (Storage) Reconcile(context.Context, []sandboxtypes.NodeSandbox, string) []sandboxtypes.NodeSandbox {
	return nil
}

// Reservations refuses every sandbox start.
type Reservations struct{}

var _ sandboxtypes.ReservationStorage = Reservations{}

func (Reservations) Reserve(context.Context, uuid.UUID, string, int) (func(sandboxtypes.Sandbox, error), func(context.Context) (sandboxtypes.Sandbox, error), error) {
	return nil, nil, ErrRedisDisabled
}

func (Reservations) Release(context.Context, uuid.UUID, string) error { return nil }

// Catalog is an empty routing catalog.
type Catalog struct{}

var _ catalog.SandboxesCatalog = Catalog{}

func (Catalog) GetSandbox(context.Context, string) (*catalog.SandboxInfo, error) {
	return nil, catalog.ErrSandboxNotFound
}

func (Catalog) StoreSandbox(context.Context, string, *catalog.SandboxInfo, time.Duration) error {
	return ErrRedisDisabled
}

func (Catalog) DeleteSandbox(context.Context, string, string) error { return nil }

func (Catalog) Close(context.Context) error { return nil }
