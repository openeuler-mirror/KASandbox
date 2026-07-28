package permissions

import (
	"fmt"
	"os/user"

	"github.com/e2b-dev/infra/packages/envd/internal/platform"
)

func GetUserIdUints(u *user.User) (uid, gid uint32, err error) {
	return platform.UserIDUints(u)
}

func GetUserIdInts(u *user.User) (uid, gid int, err error) {
	return platform.UserIDInts(u)
}

func GetUser(username string) (u *user.User, err error) {
	u, err = user.Lookup(username)
	if err != nil {
		return nil, fmt.Errorf("error looking up user '%s': %w", username, err)
	}

	return u, nil
}
