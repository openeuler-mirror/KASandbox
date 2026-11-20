package sandbox

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/vmm"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

const (
	egressCAWindowsGuestCertPath   = `C:\ProgramData\E2B\certs\egress-ca.cer`
	egressCAWindowsPendingCertPath = `C:\ProgramData\E2B\certs\egress-ca.pending.cer`
)

// injectEgressProxyCAWindows 使用 envd 默认用户导入机器级根证书库。
// envd 进程须已有写入 LocalMachine\Root 的权限。每次允许覆盖证书文件，
// 是否导入由实际信任库内的 DER SHA-256 指纹决定，恢复/轮转时保持幂等。
func injectEgressProxyCAWindows(ctx context.Context, envdBaseURL, accessToken string, certPEM []byte) error {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return fmt.Errorf("proxy CA cert is not valid PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse proxy CA cert: %w", err)
	}
	if !cert.IsCA {
		return fmt.Errorf("proxy CA cert is not a CA certificate")
	}
	fingerprint := sha256.Sum256(cert.Raw)
	wantFP := hex.EncodeToString(fingerprint[:])

	if err := envdWriteFile(ctx, envdBaseURL, accessToken, egressCAWindowsPendingCertPath, "", cert.Raw); err != nil {
		return fmt.Errorf("write Windows guest CA cert: %w", err)
	}

	command := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$expectedFingerprint = '%s'
$managedPath = '%s'
$pendingPath = '%s'
$sha256 = [System.Security.Cryptography.SHA256]::Create()
try {
    function Get-CertFileFingerprint($path) {
        $cert = New-Object System.Security.Cryptography.X509Certificates.X509Certificate2 -ArgumentList $path
        try {
            [System.BitConverter]::ToString($sha256.ComputeHash($cert.RawData)).Replace('-', '')
        } finally {
            $cert.Dispose()
        }
    }
    function Find-EgressCA($fingerprint) {
        Get-ChildItem -LiteralPath 'Cert:\LocalMachine\Root' | Where-Object {
            [System.BitConverter]::ToString($sha256.ComputeHash($_.RawData)).Replace('-', '') -eq $fingerprint
        } | Select-Object -First 1
    }
    $oldFingerprint = $null
    $hasManagedFile = Test-Path -LiteralPath $managedPath
    if ($hasManagedFile) {
        $oldFingerprint = Get-CertFileFingerprint $managedPath
    }
    if ((Get-CertFileFingerprint $pendingPath) -ne $expectedFingerprint) {
        throw 'Pending egress proxy CA fingerprint mismatch'
    }
    if ($null -eq (Find-EgressCA $expectedFingerprint)) {
        Import-Certificate -FilePath $pendingPath -CertStoreLocation 'Cert:\LocalMachine\Root' | Out-Null
    }
    $trusted = Find-EgressCA $expectedFingerprint
    if ($null -eq $trusted) {
        throw 'Egress proxy CA not found in LocalMachine\Root after import'
    }
    if ($oldFingerprint -and $oldFingerprint -ne $expectedFingerprint) {
        $old = Find-EgressCA $oldFingerprint
        if ($null -ne $old) {
            Remove-Item -LiteralPath $old.PSPath -Force -ErrorAction Stop
        }
        if ($null -ne (Find-EgressCA $oldFingerprint)) {
            throw 'Previous egress proxy CA still present in LocalMachine\Root'
        }
    }
    $trusted = Find-EgressCA $expectedFingerprint
    if ($null -eq $trusted) {
        throw 'Current egress proxy CA missing after cleanup'
    }
    # Keep the previous-generation record until import and cleanup succeed.
    if ($hasManagedFile) {
        # PowerShell converts $null to an empty string; Replace needs a null backup path.
        [System.IO.File]::Replace($pendingPath, $managedPath, [System.Management.Automation.Language.NullString]::Value)
    } else {
        [System.IO.File]::Move($pendingPath, $managedPath)
    }
    $fingerprint = [System.BitConverter]::ToString($sha256.ComputeHash($trusted.RawData)).Replace('-', '')
    Write-Output $fingerprint
} finally {
    $sha256.Dispose()
}
`, wantFP, egressCAWindowsGuestCertPath, egressCAWindowsPendingCertPath)
	output, err := runEgressCAGuestCommand(ctx, envdBaseURL, accessToken, command, vmm.OsWindows)
	if err != nil {
		return fmt.Errorf("import or verify Windows guest CA cert: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(output), wantFP) {
		return fmt.Errorf("verify Windows guest CA fingerprint: expected %s, got %q", wantFP, utils.Truncate(output, 200))
	}

	logger.L().Info(ctx, "egress proxy CA trusted in Windows LocalMachine root store",
		zap.String("fingerprint", wantFP),
		zap.String("subject", cert.Subject.String()),
	)

	return nil
}
