$ErrorActionPreference = "Stop"

$sqlPath = Join-Path $PSScriptRoot "seed_pagination_data.sql"

docker compose up -d postgres
Get-Content -Raw -Encoding UTF8 $sqlPath | docker compose exec -T postgres psql -U spring_code_bot -d spring_code_passes -v ON_ERROR_STOP=1

Write-Host "100 pagination demo requests seeded into PostgreSQL."
