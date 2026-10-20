param(
	[string]$Version = "v0.0.2",
	[string]$Commit = "",
	[string]$WindowsZip = "",
	[string]$WindowsInstaller = "",
	[string]$DockerImage = "",
	[string]$DockerTar = "",
	[switch]$SkipWindows,
	[switch]$SkipDocker,
	[switch]$SkipInstaller
)

$ErrorActionPreference = "Stop"

function Write-Step($Message) {
	Write-Host ""
	Write-Host "==> $Message"
}

function Fail($Message) {
	throw "release acceptance failed: $Message"
}

function Assert-True($Condition, $Message) {
	if (-not $Condition) {
		Fail $Message
	}
}

function New-AcceptanceRoot($Name) {
	$root = Join-Path $env:TEMP ("xuva-release-acceptance-" + $Name + "-" + [guid]::NewGuid().ToString("N"))
	New-Item -ItemType Directory -Path $root -Force | Out-Null
	return $root
}

function New-SampleMediaTree {
	param(
		[string]$Root,
		[switch]$Docker
	)
	$movies = Join-Path $Root "Movies"
	$tv = Join-Path $Root "TV"
	New-Item -ItemType Directory -Path (Join-Path $movies "Arrival (2016)") -Force | Out-Null
	New-Item -ItemType Directory -Path (Join-Path $tv "The Bear\Season 01") -Force | Out-Null
	Set-Content -LiteralPath (Join-Path $movies "Arrival (2016)\Arrival.2016.1080p.mkv") -Value "xuva acceptance fake movie" -Encoding ASCII
	Set-Content -LiteralPath (Join-Path $tv "The Bear\Season 01\The.Bear.S01E01.System.mkv") -Value "xuva acceptance fake episode" -Encoding ASCII
	if ($Docker) {
		return @{
			HostMovies = $movies
			HostTV = $tv
			Movies = "/media/Movies"
			TV = "/media/TV"
		}
	}
	return @{
		HostMovies = $movies
		HostTV = $tv
		Movies = $movies
		TV = $tv
	}
}

function ConvertTo-JsonBody($Body) {
	if ($null -eq $Body) {
		return $null
	}
	return ($Body | ConvertTo-Json -Depth 20 -Compress)
}

function Invoke-XuvaRequest {
	param(
		[string]$BaseUrl,
		[string]$Path,
		[string]$Method = "GET",
		[object]$Body = $null,
		[string]$Token = "",
		[int[]]$Expected = @(200)
	)
	$headers = @{
		"Accept" = "application/json"
	}
	if ($Token -ne "") {
		$headers["Authorization"] = "Bearer $Token"
	}
	$uri = $BaseUrl.TrimEnd("/") + $Path
	$jsonBody = ConvertTo-JsonBody $Body
	try {
		$params = @{
			Uri = $uri
			Method = $Method
			Headers = $headers
			UseBasicParsing = $true
			TimeoutSec = 20
		}
		if ($null -ne $jsonBody) {
			$params["ContentType"] = "application/json"
			$params["Body"] = $jsonBody
		}
		$response = Invoke-WebRequest @params
		$status = [int]$response.StatusCode
		$content = [string]$response.Content
	} catch {
		$response = $_.Exception.Response
		if ($null -eq $response) {
			throw
		}
		$status = [int]$response.StatusCode
		$reader = New-Object System.IO.StreamReader($response.GetResponseStream())
		$content = $reader.ReadToEnd()
	}
	if ($Expected -notcontains $status) {
		Fail "$Method $Path returned HTTP $status, expected $($Expected -join ", "): $content"
	}
	$parsed = $null
	if ($content.Trim() -ne "") {
		try {
			$parsed = $content | ConvertFrom-Json
		} catch {
			$parsed = $content
		}
	}
	return @{
		Status = $status
		Json = $parsed
		Body = $content
	}
}

function Get-NoRedirect {
	param([string]$Url)
	$request = [System.Net.HttpWebRequest]::Create($Url)
	$request.AllowAutoRedirect = $false
	$request.Method = "GET"
	try {
		$response = $request.GetResponse()
	} catch [System.Net.WebException] {
		$response = $_.Exception.Response
	}
	if ($null -eq $response) {
		throw
	}
	return @{
		Status = [int]$response.StatusCode
		Location = [string]$response.Headers["Location"]
	}
}

function Wait-XuvaReady($BaseUrl) {
	$deadline = (Get-Date).AddSeconds(90)
	$last = $null
	while ((Get-Date) -lt $deadline) {
		try {
			$response = Invoke-XuvaRequest -BaseUrl $BaseUrl -Path "/api/system/version" -Expected @(200)
			if ($response.Json.version) {
				return $response.Json
			}
		} catch {
			$last = $_
			Start-Sleep -Milliseconds 750
		}
	}
	Fail "server did not become ready at $BaseUrl; last error: $last"
}

function Wait-ScanComplete($BaseUrl, $Token, $ScanId) {
	$deadline = (Get-Date).AddSeconds(60)
	while ((Get-Date) -lt $deadline) {
		$response = Invoke-XuvaRequest -BaseUrl $BaseUrl -Path "/api/scans/$ScanId" -Token $Token -Expected @(200)
		$status = [string]$response.Json.status
		if ($status -eq "completed") {
			return $response.Json
		}
		if ($status -eq "failed") {
			Fail "scan $ScanId failed: $($response.Json.error)"
		}
		Start-Sleep -Milliseconds 500
	}
	Fail "scan $ScanId did not complete"
}

function Complete-XuvaAcceptanceFlow {
	param(
		[string]$BaseUrl,
		[string]$ExpectedVersion,
		[string]$ExpectedCommit,
		[string]$MoviesPath,
		[string]$TVPath,
		[string]$ServerName
	)

	$version = Wait-XuvaReady $BaseUrl
	Assert-True ($version.version -eq $ExpectedVersion) "expected version $ExpectedVersion, got $($version.version)"
	if ($ExpectedCommit -ne "") {
		Assert-True ($version.commit -eq $ExpectedCommit) "expected commit $ExpectedCommit, got $($version.commit)"
	}

	$root = Get-NoRedirect ($BaseUrl + "/")
	Assert-True (($root.Status -ge 300 -and $root.Status -lt 400) -or $root.Status -eq 200) "unexpected root status $($root.Status)"

	$status = Invoke-XuvaRequest -BaseUrl $BaseUrl -Path "/api/setup/status" -Expected @(200)
	Assert-True ([bool]$status.Json.requiresSetup) "blank runtime should require setup"

	$bootstrap = Invoke-XuvaRequest -BaseUrl $BaseUrl -Path "/api/client/bootstrap" -Expected @(200)
	Assert-True ([bool]$bootstrap.Json.auth.bootstrapAllowed) "blank runtime should allow first admin bootstrap"

	$password = "Acceptance-Password-123!"
	$created = Invoke-XuvaRequest -BaseUrl $BaseUrl -Path "/api/auth/bootstrap" -Method "POST" -Body @{
		username = "owner"
		password = $password
		displayName = "Owner"
	} -Expected @(201)
	$token = [string]$created.Json.sessionToken
	Assert-True ($token -ne "") "bootstrap did not return a session token"

	Invoke-XuvaRequest -BaseUrl $BaseUrl -Path "/api/settings" -Method "PUT" -Token $token -Body @{
		serverName = $ServerName
	} -Expected @(200) | Out-Null

	Invoke-XuvaRequest -BaseUrl $BaseUrl -Path "/api/setup/complete" -Method "POST" -Token $token -Body @{
		country = "AU"
		timezone = "Australia/Sydney"
	} -Expected @(200) | Out-Null

	$movieLib = Invoke-XuvaRequest -BaseUrl $BaseUrl -Path "/api/libraries" -Method "POST" -Token $token -Body @{
		name = "Acceptance Movies"
		kind = "movies"
		path = $MoviesPath
	} -Expected @(200)
	$tvLib = Invoke-XuvaRequest -BaseUrl $BaseUrl -Path "/api/libraries" -Method "POST" -Token $token -Body @{
		name = "Acceptance TV"
		kind = "tv"
		path = $TVPath
	} -Expected @(200)
	Assert-True ($movieLib.Json.id -ne "") "movie library did not return id"
	Assert-True ($tvLib.Json.id -ne "") "tv library did not return id"

	$movieScan = Invoke-XuvaRequest -BaseUrl $BaseUrl -Path ("/api/libraries/" + [uri]::EscapeDataString([string]$movieLib.Json.id) + "/scan") -Method "POST" -Token $token -Body @{} -Expected @(202)
	$movieScanDone = Wait-ScanComplete -BaseUrl $BaseUrl -Token $token -ScanId ([string]$movieScan.Json.id)
	Assert-True ([int]$movieScanDone.MediaFiles -ge 1) "movie scan should find at least one media file"

	$tvScan = Invoke-XuvaRequest -BaseUrl $BaseUrl -Path ("/api/libraries/" + [uri]::EscapeDataString([string]$tvLib.Json.id) + "/scan") -Method "POST" -Token $token -Body @{} -Expected @(202)
	$tvScanDone = Wait-ScanComplete -BaseUrl $BaseUrl -Token $token -ScanId ([string]$tvScan.Json.id)
	Assert-True ([int]$tvScanDone.MediaFiles -ge 1) "tv scan should find at least one media file"

	$libraries = Invoke-XuvaRequest -BaseUrl $BaseUrl -Path "/api/libraries" -Token $token -Expected @(200)
	Assert-True (($libraries.Json.libraries | Measure-Object).Count -ge 2) "expected libraries to persist in API"

	$summary = Invoke-XuvaRequest -BaseUrl $BaseUrl -Path "/api/catalog/summary" -Token $token -Expected @(200)
	Assert-True ([int]$summary.Json.mediaSources -ge 2 -or [int]$summary.Json.movies -ge 1 -or [int]$summary.Json.episodes -ge 1) "catalog summary did not show scanned media"

	$setupAfter = Invoke-XuvaRequest -BaseUrl $BaseUrl -Path "/api/setup/status" -Expected @(200)
	Assert-True (-not [bool]$setupAfter.Json.requiresSetup) "setup should be complete after first-run flow"

	$login = Invoke-XuvaRequest -BaseUrl $BaseUrl -Path "/api/auth/login" -Method "POST" -Body @{
		username = "owner"
		password = $password
	} -Expected @(200)
	Assert-True ([string]$login.Json.sessionToken -ne "") "login after setup failed"
	return @{
		Token = [string]$login.Json.sessionToken
		MovieLibraryID = [string]$movieLib.Json.id
		TVLibraryID = [string]$tvLib.Json.id
	}
}

function Assert-PersistenceAfterRestart {
	param(
		[string]$BaseUrl,
		[string]$ExpectedVersion
	)
	Wait-XuvaReady $BaseUrl | Out-Null
	$login = Invoke-XuvaRequest -BaseUrl $BaseUrl -Path "/api/auth/login" -Method "POST" -Body @{
		username = "owner"
		password = "Acceptance-Password-123!"
	} -Expected @(200)
	$token = [string]$login.Json.sessionToken
	$setup = Invoke-XuvaRequest -BaseUrl $BaseUrl -Path "/api/setup/status" -Expected @(200)
	Assert-True (-not [bool]$setup.Json.requiresSetup) "setup state did not persist after restart"
	$libraries = Invoke-XuvaRequest -BaseUrl $BaseUrl -Path "/api/libraries" -Token $token -Expected @(200)
	Assert-True (($libraries.Json.libraries | Measure-Object).Count -ge 2) "libraries did not persist after restart"
	$version = Invoke-XuvaRequest -BaseUrl $BaseUrl -Path "/api/system/version" -Expected @(200)
	Assert-True ($version.Json.version -eq $ExpectedVersion) "version changed after restart"
}

function Stop-ProcessTreeByRoot($Root) {
	Get-CimInstance Win32_Process |
		Where-Object { $_.CommandLine -and $_.CommandLine.Contains($Root) } |
		ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
}

function Remove-DockerContainerQuiet($Name) {
	if ($Name -eq "") {
		return
	}
	cmd /c "docker rm -f $Name >NUL 2>NUL" | Out-Null
}

function Test-WindowsPortable {
	param([string]$ZipPath)
	Write-Step "Windows portable acceptance"
	Assert-True (Test-Path $ZipPath -PathType Leaf) "Windows zip not found: $ZipPath"
	$root = New-AcceptanceRoot "win"
	$extract = Join-Path $root "app"
	$runtime = Join-Path $root "runtime"
	$media = Join-Path $root "media"
	New-Item -ItemType Directory -Path $extract, $runtime, $media -Force | Out-Null
	$tree = New-SampleMediaTree $media
	Expand-Archive -Path $ZipPath -DestinationPath $extract -Force
	$exe = Join-Path $extract "Xuva.exe"
	Assert-True (Test-Path $exe -PathType Leaf) "Xuva.exe missing from portable package"

	$old = @{}
	foreach ($name in @("XUVA_RUNTIME_HOME", "XUVA_HTTP_ADDR", "XUVA_DESKTOP_LOCAL_URL", "XUVA_LOG_LEVEL")) {
		$old[$name] = [Environment]::GetEnvironmentVariable($name, "Process")
	}
	try {
		$env:XUVA_RUNTIME_HOME = $runtime
		$env:XUVA_HTTP_ADDR = "127.0.0.1:18121"
		$env:XUVA_DESKTOP_LOCAL_URL = "http://127.0.0.1:18121"
		$env:XUVA_LOG_LEVEL = "info"
		$proc = Start-Process -FilePath $exe -PassThru -WindowStyle Hidden
		Complete-XuvaAcceptanceFlow -BaseUrl "http://127.0.0.1:18121" -ExpectedVersion $Version -ExpectedCommit $Commit -MoviesPath $tree.Movies -TVPath $tree.TV -ServerName "Acceptance Windows" | Out-Null
		foreach ($dir in @("data", "logs", "transcode", "downloads", "metadata", "cache", "temp", "trailers")) {
			Assert-True (Test-Path (Join-Path $runtime $dir) -PathType Container) "missing runtime directory: $dir"
		}
		$logFile = Join-Path $runtime "logs\xuva.ndjson"
		Assert-True (Test-Path $logFile -PathType Leaf) "structured log file missing"
		Assert-True ((Get-Content -LiteralPath $logFile -TotalCount 1) -match "^\{") "structured log file is not JSONL"
		Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
		Stop-ProcessTreeByRoot $root
		Start-Sleep -Seconds 1
		$proc = Start-Process -FilePath $exe -PassThru -WindowStyle Hidden
		Assert-PersistenceAfterRestart -BaseUrl "http://127.0.0.1:18121" -ExpectedVersion $Version
	} finally {
		if ($proc -and -not $proc.HasExited) {
			Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
		}
		Stop-ProcessTreeByRoot $root
		foreach ($entry in $old.GetEnumerator()) {
			[Environment]::SetEnvironmentVariable($entry.Key, $entry.Value, "Process")
		}
		Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
	}
}

function Test-WindowsInstaller {
	param([string]$InstallerPath)
	Write-Step "Windows installer acceptance"
	Assert-True (Test-Path $InstallerPath -PathType Leaf) "Windows installer not found: $InstallerPath"
	$root = New-AcceptanceRoot "installer"
	$installDir = Join-Path $root "install"
	$runtime = Join-Path $root "runtime"
	$media = Join-Path $root "media"
	New-Item -ItemType Directory -Path $installDir, $runtime, $media -Force | Out-Null
	$tree = New-SampleMediaTree $media
	try {
		$installArgs = @("/S", "/D=$installDir")
		$install = Start-Process -FilePath $InstallerPath -ArgumentList $installArgs -Wait -PassThru -WindowStyle Hidden
		Assert-True ($install.ExitCode -eq 0) "installer exited with $($install.ExitCode)"
		$exe = Join-Path $installDir "Xuva.exe"
		Assert-True (Test-Path $exe -PathType Leaf) "installed Xuva.exe missing at $exe"
		$oldRuntime = $env:XUVA_RUNTIME_HOME
		$oldAddr = $env:XUVA_HTTP_ADDR
		$oldLocal = $env:XUVA_DESKTOP_LOCAL_URL
		$env:XUVA_RUNTIME_HOME = $runtime
		$env:XUVA_HTTP_ADDR = "127.0.0.1:18122"
		$env:XUVA_DESKTOP_LOCAL_URL = "http://127.0.0.1:18122"
		$proc = Start-Process -FilePath $exe -PassThru -WindowStyle Hidden
		Complete-XuvaAcceptanceFlow -BaseUrl "http://127.0.0.1:18122" -ExpectedVersion $Version -ExpectedCommit $Commit -MoviesPath $tree.Movies -TVPath $tree.TV -ServerName "Acceptance Installer" | Out-Null
	} finally {
		if ($proc -and -not $proc.HasExited) {
			Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
		}
		Stop-ProcessTreeByRoot $root
		$uninstaller = Join-Path $installDir "Uninstall Xuva.exe"
		if (Test-Path $uninstaller -PathType Leaf) {
			Start-Process -FilePath $uninstaller -ArgumentList @("/S") -Wait -WindowStyle Hidden -ErrorAction SilentlyContinue
		}
		$env:XUVA_RUNTIME_HOME = $oldRuntime
		$env:XUVA_HTTP_ADDR = $oldAddr
		$env:XUVA_DESKTOP_LOCAL_URL = $oldLocal
		Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
	}
}

function Test-DockerImage {
	param([string]$Image, [string]$TarPath)
	Write-Step "Docker acceptance"
	if ($TarPath -ne "") {
		Assert-True (Test-Path $TarPath -PathType Leaf) "Docker tar not found: $TarPath"
		docker load -i $TarPath | Out-Host
	}
	Assert-True ($Image -ne "") "Docker image is required"
	$root = New-AcceptanceRoot "docker"
	$data = Join-Path $root "data"
	$media = Join-Path $root "media"
	New-Item -ItemType Directory -Path $data, $media -Force | Out-Null
	$tree = New-SampleMediaTree -Root $media -Docker
	$name = "xuva-acceptance-" + [guid]::NewGuid().ToString("N")
	try {
		Remove-DockerContainerQuiet $name
		docker run -d --name $name -p 18123:8097 -v "${data}:/data" -v "${media}:/media:ro" $Image | Out-Null
		Complete-XuvaAcceptanceFlow -BaseUrl "http://127.0.0.1:18123" -ExpectedVersion $Version -ExpectedCommit $Commit -MoviesPath $tree.Movies -TVPath $tree.TV -ServerName "Acceptance Docker" | Out-Null
		docker rm -f $name | Out-Null
		docker run -d --name $name -p 18123:8097 -v "${data}:/data" -v "${media}:/media:ro" $Image | Out-Null
		Assert-PersistenceAfterRestart -BaseUrl "http://127.0.0.1:18123" -ExpectedVersion $Version
		$logFile = Join-Path $data "logs\xuva.ndjson"
		Assert-True (Test-Path $logFile -PathType Leaf) "Docker structured log file missing"
	} finally {
		Remove-DockerContainerQuiet $name
		Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
	}
}

if ($Commit -eq "") {
	$Commit = (git rev-parse HEAD).Trim()
}
if ($WindowsZip -eq "") {
	$WindowsZip = Join-Path (Resolve-Path ".") "dist\windows\xuva-$Version-win-x64.zip"
}
if ($WindowsInstaller -eq "") {
	$WindowsInstaller = Join-Path (Resolve-Path ".") "dist\windows\xuva-$Version-win-x64.exe"
}
if ($DockerImage -eq "") {
	$DockerImage = "ghcr.io/ejm-digital/xuva:$Version"
}

Write-Step "Release acceptance for $Version at $Commit"
if (-not $SkipWindows) {
	Test-WindowsPortable -ZipPath $WindowsZip
	if (-not $SkipInstaller) {
		Test-WindowsInstaller -InstallerPath $WindowsInstaller
	}
}
if (-not $SkipDocker) {
	Test-DockerImage -Image $DockerImage -TarPath $DockerTar
}

Write-Host ""
Write-Host "Release acceptance passed for $Version"
