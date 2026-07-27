# Windows equivalent of configure.sh.
#
# A Windows guest image already ships with its user account and an auto-starting
# envd, so the Linux-only user/sudo/skeleton setup does not apply. The only
# portable, meaningful step is writing the .e2b build marker so the guest can
# identify the env/template/build it was provisioned from.
$ErrorActionPreference = "Stop"

Write-Output "Starting configuration script (Windows)"

$marker = @"
ENV_ID={{ .TemplateID }}
TEMPLATE_ID={{ .TemplateID }}
BUILD_ID={{ .BuildID }}
"@

$markerPath = Join-Path $env:SystemDrive ".e2b"
Set-Content -Path $markerPath -Value $marker -Encoding ASCII

Write-Output "Wrote build marker to $markerPath"
Write-Output "Finished configuration script (Windows)"
