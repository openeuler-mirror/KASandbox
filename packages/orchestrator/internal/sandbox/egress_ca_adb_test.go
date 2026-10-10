package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 固定测试 CA（CN=e2b-egress-test-ca），其 openssl subject_hash_old =
// e5d55a24（已用 openssl x509 -subject_hash_old 实测核对）。
const testAndroidCACertPEM = `-----BEGIN CERTIFICATE-----
MIIDGzCCAgOgAwIBAgIUUmW1o/HTsBiKYCGCTcORNRpyO6IwDQYJKoZIhvcNAQEL
BQAwHTEbMBkGA1UEAwwSZTJiLWVncmVzcy10ZXN0LWNhMB4XDTI2MTAwOTA4NDYw
NVoXDTM2MTAwNjA4NDYwNVowHTEbMBkGA1UEAwwSZTJiLWVncmVzcy10ZXN0LWNh
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAyEOOAUG50fLDUSTnauXD
vNvPKhrTkWp5KPvehWjtKMPl/bBxWaN2gm/b0MSREi2MZcXBUNhHkzkMI751n9J/
mD5OUEvy6SMxtZD38R45XxdJUsIBXUeZpfYjRHRSxg2vJLU7sJSQj1r74BYVKxR1
ucHTHY64yNMc4oh/OHyyNzR/gdBEE1HyODyn1CyGGa6XBHh+o2XIy3CE5upbUHZn
dojSTcWQn9Dhy6WFHAgC5LXH0QMH5R1vORdnE9cDKSfzRgAuO37fGM2KeW57TC3O
uqAX7fbubmG4j3QUpd9Fg4EPgc/K07cx7vh1y82IcUm+yUG7ChTB6pz+FMFIO2We
9wIDAQABo1MwUTAdBgNVHQ4EFgQUCJMwR/bP2rF94d53uBNHlm4jFRgwHwYDVR0j
BBgwFoAUCJMwR/bP2rF94d53uBNHlm4jFRgwDwYDVR0TAQH/BAUwAwEB/zANBgkq
hkiG9w0BAQsFAAOCAQEAqhHKYp4bIDbfvx/SXlHlMMD8SVQJs0s3w/JC2RI0b2Z0
iOkcQ7p0iY4Wom4IOP3epuosv0RUeGApGo7HJcYYDoJTdpWFXtXGmEhzIVddyDTc
k07FqcxQ9zm1XZH2Czao+D0uNXYrcaYStMbIlRnQuDLDOFR3p8oStjGsFv3PzUPe
0ZZP96GICKhaltLusBDa3fALpl5rB2OnWeyAzTHa7myGE8HTX+MSvw9kz4zDIS1p
ywMeR8nqnsMb3r2paO7isS6RZZGHrnf7L00ULsq9Wr/0YBjzYiIsH0eXmbhYb5aJ
+FLD+0/P4YXrXSteJ8qEC5DckfFl/GBTZoD89tLI2g==
-----END CERTIFICATE-----
`

// stubADB 记录执行序列并按规则回放输出。
type stubADB struct {
	calls   []string
	handler func(args []string) (string, error)
}

func (s *stubADB) run(_ context.Context, _ string, args ...string) (string, error) {
	s.calls = append(s.calls, strings.Join(args, " "))
	if s.handler == nil {
		return "", nil
	}

	return s.handler(args)
}

func withStubADB(t *testing.T, stub *stubADB) {
	t.Helper()
	orig := egressCAADBRun
	egressCAADBRun = stub.run
	t.Cleanup(func() { egressCAADBRun = orig })
}

func TestAndroidCAFileName(t *testing.T) {
	t.Parallel()

	name, err := androidCAFileName([]byte(testAndroidCACertPEM))
	require.NoError(t, err)
	assert.Equal(t, "e5d55a24.0", name)

	_, err = androidCAFileName([]byte("not a pem"))
	require.Error(t, err)
}

func TestInjectEgressProxyCAAndroidSkipsOnFingerprintMatch(t *testing.T) {
	stub := &stubADB{handler: func(args []string) (string, error) {
		if len(args) >= 2 && args[0] == "exec-out" && args[1] == "cat" {
			return testAndroidCACertPEM, nil
		}

		return "", nil
	}}
	withStubADB(t, stub)

	err := injectEgressProxyCAAndroid(t.Context(), "127.0.0.1:5555", []byte(testAndroidCACertPEM))
	require.NoError(t, err)

	// 只应有 connect + wait-for-device + 幂等探测 cat，不得有写操作。
	assert.Equal(t, []string{
		"connect 127.0.0.1:5555",
		"wait-for-device",
		"exec-out cat /system/etc/security/cacerts/e5d55a24.0",
	}, stub.calls)
}

func TestInjectEgressProxyCAAndroidFullSequence(t *testing.T) {
	catCalls := 0
	stub := &stubADB{handler: func(args []string) (string, error) {
		switch {
		case len(args) >= 2 && args[0] == "exec-out" && args[1] == "cat":
			catCalls++
			if catCalls == 1 {
				return "", errors.New("No such file or directory") // 幂等探测：未安装
			}

			return testAndroidCACertPEM, nil // 读回校验
		case args[0] == "root":
			return "adbd is already running as root\n", nil
		default:
			return "", nil
		}
	}}
	withStubADB(t, stub)

	err := injectEgressProxyCAAndroid(t.Context(), "127.0.0.1:5555", []byte(testAndroidCACertPEM))
	require.NoError(t, err)

	joined := strings.Join(stub.calls, "\n")
	assert.Contains(t, joined, "root")
	assert.Contains(t, joined, "remount")
	assert.Contains(t, joined, "push")
	assert.Contains(t, joined, "shell chmod 0644 /system/etc/security/cacerts/e5d55a24.0")
	assert.Contains(t, joined, "shell restorecon /system/etc/security/cacerts/e5d55a24.0")
	// root 已在位时不允许出现 disconnect 重连。
	assert.NotContains(t, joined, "disconnect")
}

func TestInjectEgressProxyCAAndroidReconnectsAfterRootRestart(t *testing.T) {
	connects := 0
	catCalls := 0
	stub := &stubADB{handler: func(args []string) (string, error) {
		switch args[0] {
		case "exec-out":
			catCalls++
			if catCalls == 1 {
				return "", errors.New("No such file or directory") // 幂等探测：未安装
			}

			return testAndroidCACertPEM, nil // 读回校验
		case "root":
			return "restarting adbd as root\n", nil
		case "connect":
			connects++
			if connects == 2 {
				return "", errors.New("connection refused") // 第一次重连失败
			}

			return "connected\n", nil
		default:
			return "", nil
		}
	}}
	withStubADB(t, stub)

	err := injectEgressProxyCAAndroid(t.Context(), "127.0.0.1:5555", []byte(testAndroidCACertPEM))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, connects, 3) // 首次 connect + 至少两次重连
	assert.Contains(t, strings.Join(stub.calls, "\n"), "disconnect 127.0.0.1:5555")
}

func TestInjectEgressProxyCAAndroidRootDeniedFailsFast(t *testing.T) {
	stub := &stubADB{handler: func(args []string) (string, error) {
		if args[0] == "exec-out" {
			return "", errors.New("No such file or directory")
		}
		if args[0] == "root" {
			return "adbd cannot run as root in production builds\n", errors.New("closed")
		}

		return "", nil
	}}
	withStubADB(t, stub)

	err := injectEgressProxyCAAndroid(t.Context(), "127.0.0.1:5555", []byte(testAndroidCACertPEM))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "内置 CA")

	// root 被拒后不得继续 remount/push。
	for _, call := range stub.calls {
		assert.False(t, strings.HasPrefix(call, "remount") || strings.HasPrefix(call, "push"), "unexpected call after root denial: %s", call)
	}
}

func TestInjectEgressProxyCAAndroidRemountFailureRollsBack(t *testing.T) {
	stub := &stubADB{handler: func(args []string) (string, error) {
		switch args[0] {
		case "shell":
			return "", errors.New("No such file or directory")
		case "root":
			return "adbd is already running as root\n", nil
		case "remount":
			return "remount failed: Permission denied\n", fmt.Errorf("exit status 1")
		default:
			return "", nil
		}
	}}
	withStubADB(t, stub)

	err := injectEgressProxyCAAndroid(t.Context(), "127.0.0.1:5555", []byte(testAndroidCACertPEM))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remount")

	for _, call := range stub.calls {
		assert.False(t, strings.HasPrefix(call, "push"), "push must not run after remount failure")
	}
}
