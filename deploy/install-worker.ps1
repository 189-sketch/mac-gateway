# Installs or upgrades the worker on Windows. Run it in PowerShell opened with
# "Run as administrator":
#
#   From the gateway, which knows its own address:
#     irm https://GATEWAY/install/worker.ps1 | iex
#
#   From a release, which asks for the gateway's address:
#     irm RELEASE_URL/install-worker.ps1 | iex
#
# It installs the worker, then pairs it: it asks the gateway to pair, the server
# shows a code, and you type the code here. Pressing Enter at either question
# skips it, and the worker's page, which opens at the end, can pair it later. To
# pair with no questions, set $env:FLEET_NODE_TOKEN to a token made on the server
# with fleet-add-worker.
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

	# The gateway fills in the first of these when it publishes this script, and
	# build-release.sh the second. Either stays a placeholder otherwise.
	$gatewayUrl = '@@GATEWAY_URL@@'
	$releaseUrl = '@@RELEASE_URL@@'
	if ($env:FLEET_GATEWAY_URL) { $gatewayUrl = $env:FLEET_GATEWAY_URL }
	if ($env:FLEET_RELEASE_URL) { $releaseUrl = $env:FLEET_RELEASE_URL }
	# An address has no @ in it, and a placeholder nobody filled in does.
	if ($gatewayUrl -like '*@*') { $gatewayUrl = '' }
	if ($releaseUrl -notmatch '^https://') { $releaseUrl = '' }
	$taskName = 'Fleet Worker'

	function Stop-Install([string]$message) {
		throw "install-worker: $message"
	}

	# Turns what a person typed, such as 203.0.113.10, into the gateway's address.
	function Get-CleanAddress([string]$text) {
		$text = ($text -replace '\s', '').TrimEnd('/')
		if ($text -and $text -notmatch '://') { $text = "https://$text" }
		return $text
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

	if ($nodeName -and $nodeName -cnotmatch '^[A-Za-z0-9][A-Za-z0-9._-]*$') { Stop-Install "'$nodeName' isn't a valid node name" }
	if ($nodeToken -and $nodeToken -cnotmatch '^[A-Za-z0-9_]+$') { Stop-Install "FLEET_NODE_TOKEN isn't a node token; on the gateway, run: fleet-add-worker NAME" }

	# The gateway is the one this script was published with, the one already in
	# worker.yaml, or the one the person gives. An empty answer means pair later.
	$gatewayUrl = Get-CleanAddress $gatewayUrl
	if (-not $gatewayUrl -and (Test-Path $manifest)) {
		$line = Get-Content -Path $manifest | Where-Object { $_ -match '^gateway_url:' } | Select-Object -First 1
		if ($line) { $gatewayUrl = Get-CleanAddress (($line -replace '^gateway_url:\s*', '' -replace '\s*#.*$', '') -replace "[`"']", '') }
	} elseif (-not $gatewayUrl) {
		for ($try = 0; $try -lt 5 -and -not $gatewayUrl; $try++) {
			$answer = Get-CleanAddress (Read-Host "Gateway address (the server's IP address or domain; press Enter to skip)")
			if (-not $answer) { break }
			try {
				Invoke-WebRequest -UseBasicParsing -TimeoutSec 15 -Uri "$answer/healthz" | Out-Null
				$gatewayUrl = $answer
			} catch {
				Write-Host "No gateway answered at $answer. Check the address, and that the gateway's install finished. A new server's certificate can take a minute."
			}
		}
	}
	if ($gatewayUrl -and $gatewayUrl -notmatch '^https://') {
		Stop-Install "the gateway address must start with https://, got '$gatewayUrl'"
	}
	if ($gatewayUrl) { $downloadUrl = "$gatewayUrl/install" }
	elseif ($releaseUrl) { $downloadUrl = $releaseUrl }
	else { Stop-Install "there's nowhere to download the worker from. Set `$env:FLEET_GATEWAY_URL to the gateway's address and run this again" }

	New-Item -ItemType Directory -Force -Path $fleetHome | Out-Null
	if (-not $nodeName) {
		$nodeName = ($env:COMPUTERNAME -replace '[^A-Za-z0-9_-]', '') -replace '^[-_]+', ''
		if (-not $nodeName) { $nodeName = 'worker' }
		$nameGiven = $false
	} else {
		$nameGiven = $true
	}

	Write-Host "Note: this is an administrator's account. Jobs run as this user with limited rights, but can read this user's files. A dedicated standard account with no personal files is safer."

	Write-Host "==> Downloading the worker for $arch"
	$tmp = Join-Path ([IO.Path]::GetTempPath()) ("fleet-" + [Guid]::NewGuid().ToString('N'))
	New-Item -ItemType Directory -Path $tmp | Out-Null
	try {
		$download = Join-Path $tmp $binary
		Invoke-WebRequest -UseBasicParsing -Uri "$downloadUrl/$binary" -OutFile $download
		$sums = (Invoke-WebRequest -UseBasicParsing -Uri "$downloadUrl/SHA256SUMS").Content
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

		# The token file is made now, empty if there's no token yet, so its access
		# can be limited before a token is ever written to it. Pairing writes the
		# token into this same file.
		if ($nodeToken) {
			[IO.File]::WriteAllText($tokenFile, $nodeToken + "`n", (New-Object Text.ASCIIEncoding))
		} elseif (-not (Test-Path $tokenFile)) {
			[IO.File]::WriteAllText($tokenFile, '', (New-Object Text.ASCIIEncoding))
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
#
# Logs are in worker.log. For more detail, add this line and restart:
#   log_level: debug
#
# Pairing this machine fills in gateway_url and node.name.
gateway_url: "$gatewayUrl"
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
    test: { input: {} }
"@
			[IO.File]::WriteAllText($manifest, ($yaml -replace "`r`n", "`n"), (New-Object Text.ASCIIEncoding))
		}

		# Check before replacing anything that's running, so a bad config leaves the
		# current worker alone.
		& $download --config $manifest --check
		if ($LASTEXITCODE -ne 0) { Stop-Install "$manifest isn't valid" }

		# Pair, unless this machine already has a token. Whatever happens, the
		# install carries on: a machine that isn't paired can be paired from its page.
		$paired = $false
		$hasToken = (Get-Item $tokenFile).Length -gt 0
		if ($gatewayUrl -and $nodeToken -and -not $nameGiven) {
			# A token alone is enough: the gateway knows which machine it was made for.
			Write-Host "==> Connecting to $gatewayUrl with the token"
			& $download pair --config $manifest --gateway $gatewayUrl --key $nodeToken
			if ($LASTEXITCODE -eq 0) { $paired = $true }
			else { Write-Host "The token didn't work. You can pair this machine from its page instead." }
		} elseif ($gatewayUrl -and $hasToken) {
			$paired = $true
		} elseif ($gatewayUrl) {
			Write-Host "==> Pairing with $gatewayUrl"
			if ($nameGiven) { & $download pair --config $manifest --gateway $gatewayUrl --name $nodeName }
			else { & $download pair --config $manifest --gateway $gatewayUrl }
			if ($LASTEXITCODE -eq 0) { $paired = $true }
			elseif ($LASTEXITCODE -eq 3) { Write-Host 'Skipped. You can pair this machine later from its page.' }
			else { Write-Host "Pairing didn't work. The install carries on, and you can pair this machine from its page." }
		}

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

		$waitingFor = 'not paired with a gateway'
		if ($paired) { $waitingFor = 'connected to gateway' }
		$started = $false
		for ($i = 0; $i -lt 20 -and -not $started; $i++) {
			Start-Sleep -Seconds 1
			if ((Test-Path $logFile) -and (Select-String -Quiet -Path $logFile -Pattern $waitingFor)) { $started = $true }
		}
		if (-not $started) {
			if ($paired) { Write-Host "The worker started but hasn't connected. Its log says:" }
			else { Write-Host "The worker didn't start. Its log says:" }
			if (Test-Path $logFile) { Get-Content -Tail 15 -Path $logFile | Write-Host }
			Stop-Install 'the worker did not start properly'
		}

		# Opens the worker's page in the browser, signed in.
		Write-Host ''
		& $workerExe ui --config $manifest --wait 15s
		if ($LASTEXITCODE -ne 0) { Write-Host "The worker's page didn't open. Its log, $logFile, says why." }
	} finally {
		Remove-Item -Recurse -Force -Path $tmp -ErrorAction SilentlyContinue
		Remove-Item Env:FLEET_NODE_TOKEN -ErrorAction SilentlyContinue
	}

	Write-Host ''
	if ($paired) {
		Write-Host "==> Connected to $gatewayUrl"
		Write-Host "Installed in $fleetHome. The Connection tab of the worker's page has a test"
		Write-Host "that checks agents can reach this machine's tools through MCP."
	} else {
		Write-Host '==> Installed, but not paired with a gateway yet'
		Write-Host "Installed in $fleetHome. To pair it, give the gateway's address on the"
		Write-Host "Connection tab of the worker's page, or run:"
		Write-Host ''
		Write-Host "  & '$workerExe' pair --config '$manifest'"
	}
	Write-Host ''
	Write-Host "To open the worker's page again:"
	Write-Host ''
	Write-Host "  & '$workerExe' ui --config '$manifest'"
	Write-Host ''
	Write-Host "To watch what the worker is doing:"
	Write-Host ''
	Write-Host "  Get-Content -Wait -Tail 20 '$logFile'"
	Write-Host ''
	Write-Host 'To keep this PC available, run these yourself. They stop it sleeping or hibernating while on mains power:'
	Write-Host ''
	Write-Host '  powercfg /change standby-timeout-ac 0'
	Write-Host '  powercfg /change hibernate-timeout-ac 0'
}

Install-FleetWorker
