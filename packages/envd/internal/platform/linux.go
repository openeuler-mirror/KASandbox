//go:build linux && !android

package platform

import (
	"os/user"
)

func LookupUser(username string) (*user.User, error) {
	return user.Lookup(username)
}

func E2BRunDir() string {
	return "/run/e2b"
}
