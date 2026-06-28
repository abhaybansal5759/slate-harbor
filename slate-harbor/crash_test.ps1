$ErrorActionPreference = "Stop"
$base = "http://localhost:8080"
$svc = "app"   # <-- change if 'docker compose ps' shows a different name
$key = "crash-" + [Guid]::NewGuid().ToString("N")
$player = "crashp-" + [Guid]::NewGuid().ToString("N")
$h = @{ "Idempotency-Key" = $key }

# 1. credit, committed before the crash
Invoke-RestMethod -Method Post -Uri "$base/v1/wallets/$player/credit" -Headers $h -ContentType "application/json" -Body '{"amount":100,"reason":"battle"}' | Out-Null
Write-Host "credited, balance:" (Invoke-RestMethod "$base/v1/wallets/$player").balance

# 2. HARD kill (SIGKILL == kill -9)
$cid = docker compose ps -q $svc
docker kill $cid | Out-Null
Write-Host "killed $svc ($cid)"

# 3. restart and wait for readiness
docker compose up -d $svc | Out-Null
do { Start-Sleep -Seconds 1; try { $r = Invoke-RestMethod "$base/readyz" } catch { $r = $null } } until ($r.status -eq "ready")
Write-Host "service recovered"

# 4. committed credit survived exactly once
$bal = (Invoke-RestMethod "$base/v1/wallets/$player").balance
if ($bal -ne 100) { throw "DURABILITY FAIL: balance=$bal want 100" }
Write-Host "PASS: committed credit survived restart"

# 5. retry SAME key after crash => no double-credit
Invoke-RestMethod -Method Post -Uri "$base/v1/wallets/$player/credit" -Headers $h -ContentType "application/json" -Body '{"amount":100,"reason":"battle"}' | Out-Null
$bal2 = (Invoke-RestMethod "$base/v1/wallets/$player").balance
if ($bal2 -ne 100) { throw "EXACTLY-ONCE FAIL: post-crash retry made balance $bal2" }
Write-Host "PASS: post-crash retry was idempotent (still 100)"