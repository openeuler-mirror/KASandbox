//go:build android

package platform

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	PortForwardPlatformName          = "Android"
	PortForwardScannerSubscriberName = "android-port-forwarder"
	DefaultPortForwardBindIP         = "169.254.0.21"
	DefaultPortForwardDisabled       = false
)

func LookupUser(username string) (*user.User, error) {
	current, err := CurrentAndroidUser()
	if err != nil {
		return nil, err
	}

	if username == "" {
		return current, nil
	}

	if u, err := user.Lookup(username); err == nil {
		return u, nil
	} else if !isUnknownUserError(err) {
		return nil, err
	}

	if username == current.Uid {
		return current, nil
	}

	if username == "root" && current.Uid == "0" {
		return currentWithUsername(current, "root"), nil
	}

	return nil, fmt.Errorf("android envd supports only current user %q, got %q", current.Username, username)
}

func isUnknownUserError(err error) bool {
	var unknownUser user.UnknownUserError
	if errors.As(err, &unknownUser) {
		return true
	}

	var unknownID user.UnknownUserIdError
	if errors.As(err, &unknownID) {
		return true
	}

	return strings.Contains(err.Error(), "not implemented on android")
}

func currentWithUsername(current *user.User, username string) *user.User {
	u := *current
	u.Username = username
	u.Name = username

	return &u
}

func CurrentAndroidUser() (*user.User, error) {
	uid := os.Getuid()
	gid := os.Getgid()
	home := os.Getenv("E2B_HOME")
	if home == "" {
		home = os.Getenv("HOME")
	}
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			home = userHome
		}
	}
	if home == "" {
		home = "/"
	}

	username := os.Getenv("USER")
	if username == "" {
		if uid == 0 {
			username = "root"
		} else {
			username = "android"
		}
	}

	return &user.User{
		Uid:      strconv.Itoa(uid),
		Gid:      strconv.Itoa(gid),
		Username: username,
		Name:     username,
		HomeDir:  home,
	}, nil
}

func E2BRunDir() string {
	if runDir := os.Getenv("E2B_RUN_DIR"); runDir != "" {
		return runDir
	}

	home := os.Getenv("E2B_HOME")
	if home == "" {
		home = os.Getenv("HOME")
	}
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			home = userHome
		}
	}
	if home != "" && home != "/" {
		return filepath.Join(home, ".e2b", "run")
	}

	return filepath.Join(".e2b", "run")
}
