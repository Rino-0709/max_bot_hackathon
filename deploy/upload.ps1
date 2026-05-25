param(
    [Parameter(Mandatory = $true)]
    [string]$ServerHost,

    [Parameter(Mandatory = $true)]
    [string]$User,

    [int]$Port = 22,

    [string]$RemotePath = "/opt/spring-code-1"
)

$ErrorActionPreference = "Stop"

$exclude = @(
    ".env",
    ".git",
    "data",
    "node_modules",
    "dist",
    "*.exe",
    "*.test"
)

$temp = Join-Path $env:TEMP ("spring-code-1-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $temp | Out-Null

try {
    $source = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
    robocopy $source $temp /MIR /XD ".git" "data" "node_modules" "dist" /XF ".env" "*.exe" "*.test" | Out-Null
    if ($LASTEXITCODE -gt 7) {
        throw "robocopy failed with exit code $LASTEXITCODE"
    }

    $archive = "$temp.zip"
    Compress-Archive -Path (Join-Path $temp "*") -DestinationPath $archive -Force

    ssh -p $Port "$User@$ServerHost" "mkdir -p '$RemotePath'"
    if ($LASTEXITCODE -ne 0) { throw "remote mkdir failed with exit code $LASTEXITCODE" }

    scp -P $Port $archive "$User@$ServerHost`:$RemotePath/app.zip"
    if ($LASTEXITCODE -ne 0) { throw "scp failed with exit code $LASTEXITCODE" }

    ssh -p $Port "$User@$ServerHost" "cd '$RemotePath' && rm -rf .deploy_tmp && mkdir .deploy_tmp && (unzip -oq app.zip -d .deploy_tmp || [ `$? -le 1 ]) && cp -a .deploy_tmp/. . && rm -rf .deploy_tmp app.zip"
    if ($LASTEXITCODE -ne 0) { throw "remote unpack failed with exit code $LASTEXITCODE" }

    Write-Host "Uploaded to ${User}@${ServerHost}:${RemotePath}"
} finally {
    Remove-Item -Recurse -Force $temp -ErrorAction SilentlyContinue
    Remove-Item -Force "$temp.zip" -ErrorAction SilentlyContinue
}
