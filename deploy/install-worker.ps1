# Installs or upgrades the worker on Windows. The gateway publishes this script,
# and fleet-add-worker prints the command that runs it. Run that command in
# PowerShell opened with "Run as administrator".
#
# It installs into %LOCALAPPDATA%\Fleet and registers a scheduled task that runs
# the worker as this user at every boot, without anyone logging in. A task is used
# rather than a Windows service because a service would run outside this user's
# account, away from the tools and models installed there.
#
# Re-running it upgrades the binary and keeps your worker.yaml and token.
#
# Everything is inside a function, so a download cut off partway can't run half a
# script: the unfinished function fails to parse and nothing happens.

function Install-FleetWorker {
	$ErrorActionPreference = 'Stop'
	$ProgressPreference = 'SilentlyContinue'
	[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

	$gatewayUrl = '@@GATEWAY_URL@@'
	if ($env:FLEET_GATEWAY_URL) { $gatewayUrl = $env:FLEET_GATEWAY_URL }
	$taskName = 'Fleet Worker'

	function Stop-Install([string]$message) {
		throw "install-worker: $message"
	}

	if ($gatewayUrl -notmatch '^https://') {
		Stop-Install "the gateway address must start with https://, got '$gatewayUrl'"
	}
	$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
	$principal = New-Object Security.Principal.WindowsPrincipal($identity)
	if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
		Stop-Install 'open PowerShell with "Run as administrator" and run this again. Registering a task that starts at boot needs it. The worker itself runs without admin rights.'
	}

	switch ($env:PROCESSOR_ARCHITECTURE) {
		'AMD64' { $arch = 'amd64' }
		'ARM64' { $arch = 'arm64' }
		default { Stop-Install "unsupported processor: $env:PROCESSOR_ARCHITECTURE" }
	}

	$fleetHome = Join-Path $env:LOCALAPPDATA 'Fleet'
	$tokenFile = Join-Path $fleetHome 'token'
	$manifest = Join-Path $fleetHome 'worker.yaml'
	$workerExe = Join-Path $fleetHome 'fleet-worker.exe'
	$logFile = Join-Path $fleetHome 'worker.log'
	$nodeName = $env:FLEET_NODE_NAME
	$nodeToken = $env:FLEET_NODE_TOKEN
	$binary = "fleet-worker-windows-$arch.exe"

	New-Item -ItemType Directory -Force -Path $fleetHome | Out-Null
	if (-not (Test-Path $tokenFile) -and -not $nodeToken) {
		Stop-Install "FLEET_NODE_TOKEN isn't set; on the gateway, run: fleet-add-worker NAME"
	}
	if (-not (Test-Path $manifest)) {
		if (-not $nodeName) { Stop-Install "FLEET_NODE_NAME isn't set" }
		if ($nodeName -cnotmatch '^[A-Za-z0-9][A-Za-z0-9._-]*$') { Stop-Install "'$nodeName' isn't a valid node name" }
	}

	Write-Host "Note: this is an administrator's account. Jobs run as this user with limited rights, but can read this user's files. A dedicated standard account with no personal files is safer."

	Write-Host "==> Downloading the worker for $arch"
	$tmp = Join-Path ([IO.Path]::GetTempPath()) ("fleet-" + [Guid]::NewGuid().ToString('N'))
	New-Item -ItemType Directory -Path $tmp | Out-Null
	try {
		$download = Join-Path $tmp $binary
		Invoke-WebRequest -UseBasicParsing -Uri "$gatewayUrl/install/$binary" -OutFile $download
		$sums = (Invoke-WebRequest -UseBasicParsing -Uri "$gatewayUrl/install/SHA256SUMS").Content
		if ($sums -is [byte[]]) { $sums = [Text.Encoding]::ASCII.GetString($sums) }
		$expected = $null
		foreach ($line in ($sums -split "`n")) {
			$parts = $line.Trim() -split '\s+'
			if ($parts.Count -eq 2 -and $parts[1] -eq $binary) { $expected = $parts[0] }
		}
		$actual = (Get-FileHash -Algorithm SHA256 -Path $download).Hash
		if (-not $expected -or $actual -ine $expected) {
			Stop-Install "the download doesn't match its checksum"
		}

		if ($nodeToken) {
			[IO.File]::WriteAllText($tokenFile, $nodeToken + "`n", (New-Object Text.ASCIIEncoding))
		}
		# Windows has no chmod 600. This removes every inherited permission, leaving
		# only this user, so the worker doesn't check the file's mode here.
		& icacls.exe $tokenFile /inheritance:r /grant:r "${env:USERNAME}:F" | Out-Null
		if ($LASTEXITCODE -ne 0) { Stop-Install "couldn't restrict access to $tokenFile" }

		if (Test-Path $manifest) {
			Write-Host '==> Keeping your existing worker.yaml'
		} else {
			$yaml = @"
# This file alone decides what this machine will run. See the README for how to
# add services. Restart after changing it:
#   Stop-ScheduledTask -TaskName 'Fleet Worker'; Start-ScheduledTask -TaskName 'Fleet Worker'
gateway_url: $gatewayUrl
token_file: token
state_dir: state

node:
  name: $nodeName
  max_jobs: 2
  labels:
    os: windows

capabilities:
  # Harmless, and shows that jobs reach this machine. Replace it with your own.
  - name: system.hostname
    adapter: cli
    command: ['C:\Windows\System32\HOSTNAME.EXE']
    input_schema:
      type: object
      additionalProperties: false
    timeout: 1m
    retry_if_lost: true
"@
			[IO.File]::WriteAllText($manifest, ($yaml -replace "`r`n", "`n"), (New-Object Text.ASCIIEncoding))
		}

		# Check before replacing anything that's running, so a bad config leaves the
		# current worker alone.
		& $download --config $manifest --check
		if ($LASTEXITCODE -ne 0) { Stop-Install "$manifest isn't valid" }

		Write-Host '==> Starting the worker'
		if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) {
			Stop-ScheduledTask -TaskName $taskName
			Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
			Start-Sleep -Seconds 2
		}
		Move-Item -Force -Path $download -Destination $workerExe
		if (Test-Path $logFile) { Clear-Content -Path $logFile }

		$action = New-ScheduledTaskAction -Execute $workerExe -Argument "--config `"$manifest`" --log-file `"$logFile`"" -WorkingDirectory $fleetHome
		# The repeating trigger brings it back after a crash: while it's running,
		# MultipleInstances IgnoreNew makes each repeat do nothing.
		$triggers = @(
			(New-ScheduledTaskTrigger -AtStartup),
			(New-ScheduledTaskTrigger -Once -At (Get-Date) -RepetitionInterval (New-TimeSpan -Minutes 5))
		)
		$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew
		# S4U runs it with nobody logged in and stores no password. It also means no
		# access to network shares, which jobs shouldn't rely on.
		$task = New-ScheduledTaskPrincipal -UserId "$env:USERDOMAIN\$env:USERNAME" -LogonType S4U -RunLevel Limited
		Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $triggers -Settings $settings -Principal $task -Description 'Runs jobs from the Fleet gateway.' | Out-Null
		Start-ScheduledTask -TaskName $taskName

		$connected = $false
		for ($i = 0; $i -lt 20 -and -not $connected; $i++) {
			Start-Sleep -Seconds 1
			if ((Test-Path $logFile) -and (Select-String -Quiet -Path $logFile -Pattern 'connected to gateway')) { $connected = $true }
		}
		if (-not $connected) {
			Write-Host "The worker started but hasn't connected. Its log says:"
			if (Test-Path $logFile) { Get-Content -Tail 15 -Path $logFile | Write-Host }
			Stop-Install 'the worker did not connect'
		}
	} finally {
		Remove-Item -Recurse -Force -Path $tmp -ErrorAction SilentlyContinue
		Remove-Item Env:FLEET_NODE_TOKEN -ErrorAction SilentlyContinue
	}

	Write-Host ''
	Write-Host "==> Connected to $gatewayUrl"
	Write-Host "Installed in $fleetHome. The log is $logFile."
	Write-Host ''
	Write-Host 'To keep this PC available, run these yourself. They stop it sleeping or hibernating while on mains power:'
	Write-Host ''
	Write-Host '  powercfg /change standby-timeout-ac 0'
	Write-Host '  powercfg /change hibernate-timeout-ac 0'
}

Install-FleetWorker
