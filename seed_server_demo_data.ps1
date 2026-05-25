param(
    [Parameter(Mandatory = $true)]
    [string]$ServerHost,
    [string]$User = "root"
)

$ErrorActionPreference = "Stop"

$remote = "${User}@${ServerHost}"
$localSql = Join-Path $PSScriptRoot "seed_server_demo_data.sql"

scp $localSql "${remote}:/tmp/seed_server_demo_data.sql"
ssh $remote "cd /opt/spring-code-1 && docker compose exec -T postgres psql -U spring_code_bot -d spring_code_passes -v ON_ERROR_STOP=1 < /tmp/seed_server_demo_data.sql"
@'
SELECT status, COUNT(*) FROM pass_requests WHERE request_number LIKE 'PASS-DEMO-%' GROUP BY status ORDER BY status;
SELECT COUNT(*) AS demo_requests FROM pass_requests WHERE request_number LIKE 'PASS-DEMO-%';
'@ | ssh $remote "cd /opt/spring-code-1 && docker compose exec -T postgres psql -U spring_code_bot -d spring_code_passes"
