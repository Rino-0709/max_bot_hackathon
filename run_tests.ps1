$ErrorActionPreference = "Stop"

if (-not (Test-Path -LiteralPath (Join-Path $PSScriptRoot ".env"))) {
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot ".env.example") -Destination (Join-Path $PSScriptRoot ".env")
    Write-Host "Created .env from .env.example for local tests."
}

$go = Get-Command go -ErrorAction SilentlyContinue
if (-not $go) {
    $defaultGo = "C:\Program Files\Go\bin\go.exe"
    if (Test-Path -LiteralPath $defaultGo) {
        $go = Get-Item -LiteralPath $defaultGo
    } else {
        throw "Go is not found in PATH and was not found at $defaultGo"
    }
}

$docker = Get-Command docker -ErrorAction SilentlyContinue
if ($docker) {
    docker compose up -d postgres
    for ($i = 0; $i -lt 30; $i++) {
        docker compose exec -T postgres pg_isready -U spring_code_bot -d spring_code_passes | Out-Null
        if ($LASTEXITCODE -eq 0) {
            break
        }
        Start-Sleep -Seconds 1
    }
    if ($LASTEXITCODE -ne 0) {
        throw "PostgreSQL did not become ready in Docker Compose."
    }
} else {
    Write-Host "Docker is not in PATH. Tests will use DATABASE_URL from .env as-is."
}

& $go.Source test ./...
& $go.Source test ./cmd/maxbot -run TestScenario -v
$buildOut = Join-Path ([System.IO.Path]::GetTempPath()) "spring-code-1-maxbot-test.exe"
& $go.Source build -o $buildOut ./cmd/maxbot
Remove-Item -LiteralPath $buildOut -Force -ErrorAction SilentlyContinue
