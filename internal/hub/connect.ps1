# Connects this computer to the box @BOX@ with Syncthing, so a folder here
# is kept in step with your folder on the box. It installs Syncthing if it
# isn't here, starts it when you sign in, adds the box and puts the folders
# the box offers you in your user folder. Nothing to do in Syncthing itself.
# From the box at @HUB@; run it again at any time, it skips what's done.
# Run it in PowerShell as:  irm @HUB@/connect.ps1 | iex
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # downloads are much faster without the bar
# Windows PowerShell 5.1 may not offer TLS 1.2 by itself, which GitHub needs.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
$Hub = '@HUB@'
$Box = '@BOX@'
$BoxID = '@BOX_ID@'
$BoxAddrs = '@ADDRS@' -split ' '

function Fail($msg) { Write-Host $msg -ForegroundColor Red; throw $msg }

& {
  # 1. Syncthing: the one already here, or the latest release.
  $dir = Join-Path $env:LOCALAPPDATA 'Programs\Syncthing'
  $st = $null
  foreach ($c in @((Join-Path $dir 'syncthing.exe'), "$env:ProgramFiles\Syncthing\syncthing.exe")) {
    if (Test-Path $c) { $st = $c; break }
  }
  if (-not $st) { $cmd = Get-Command syncthing.exe -ErrorAction SilentlyContinue; if ($cmd) { $st = $cmd.Source } }
  if (-not $st) {
    Write-Host 'Downloading Syncthing...'
    $arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
    $rel = Invoke-RestMethod 'https://api.github.com/repos/syncthing/syncthing/releases/latest'
    $v = $rel.tag_name
    $f = "syncthing-windows-$arch-$v.zip"
    $base = "https://github.com/syncthing/syncthing/releases/download/$v"
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid())
    New-Item -ItemType Directory $tmp | Out-Null
    try {
      Invoke-WebRequest "$base/$f" -OutFile (Join-Path $tmp $f) -UseBasicParsing
      $sums = (Invoke-WebRequest "$base/sha256sum.txt.asc" -UseBasicParsing).Content
      if ($sums -is [byte[]]) { $sums = [Text.Encoding]::UTF8.GetString($sums) }
      $want = ($sums -split "`n" | Where-Object { ($_ -split '\s+')[1] -eq $f } | ForEach-Object { ($_ -split '\s+')[0] }) | Select-Object -First 1
      $got = (Get-FileHash (Join-Path $tmp $f) -Algorithm SHA256).Hash
      if (-not $want -or $want -ne $got.ToLower()) { Fail "The Syncthing download doesn't match its checksum; stopping." }
      Expand-Archive (Join-Path $tmp $f) -DestinationPath $tmp
      New-Item -ItemType Directory $dir -Force | Out-Null
      Copy-Item (Join-Path $tmp "syncthing-windows-$arch-$v\syncthing.exe") $dir -Force
    } finally { Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue }
    $st = Join-Path $dir 'syncthing.exe'
    Write-Host "Installed Syncthing $v in $st."
  }

  function Running {
    # Not Stop here: Windows PowerShell 5.1 turns Syncthing's "not running"
    # on stderr into an error that ends the script.
    $ErrorActionPreference = 'Continue'
    & $st cli show system *> $null
    return $LASTEXITCODE -eq 0
  }

  # 2. Running, and started when you sign in.
  $env:STNODEFAULTFOLDER = '1' # its first start makes no "Default Folder"
  if (-not (Running)) {
    $startup = [Environment]::GetFolderPath('Startup')
    $lnk = Join-Path $startup 'Syncthing.lnk'
    if (-not (Test-Path $lnk)) {
      $sc = (New-Object -ComObject WScript.Shell).CreateShortcut($lnk)
      $sc.TargetPath = $st
      $sc.Arguments = 'serve --no-browser --no-console'
      $sc.WorkingDirectory = Split-Path $st
      $sc.WindowStyle = 7 # minimised
      $sc.Description = "Syncthing, keeping folders in step with the box $Box"
      $sc.Save()
    }
    Start-Process $st -ArgumentList 'serve', '--no-browser', '--no-console' -WindowStyle Hidden
    Write-Host 'Starting Syncthing...'
    $n = 0
    while (-not (Running)) {
      if (++$n -ge 60) { Fail "Syncthing didn't start; try restarting the computer and running this again." }
      Start-Sleep 1
    }
  }
  $me = (& $st cli show system | ConvertFrom-Json).myID
  if (-not $me) { Fail "Couldn't read this computer's Syncthing ID." }

  # 3. The box.
  if (-not (& $st cli config devices list | Where-Object { $_ -eq $BoxID })) {
    # Addresses one by one: "devices add --addresses" crashes Syncthing 1.29.
    & $st cli config devices add --device-id $BoxID --name $Box
    if ($LASTEXITCODE -ne 0) { Fail "Couldn't add the box to Syncthing." }
    foreach ($x in $BoxAddrs) { if ($x -ne 'dynamic') { & $st cli config devices $BoxID addresses add $x } }
    Write-Host "Added the box $Box."
  }

  # 4. The folders it offers you: it takes this computer within half a
  # minute of it connecting.
  Write-Host 'Waiting for the box to take this computer...'
  $lines = @()
  for ($n = 0; ; $n++) {
    try {
      $body = (Invoke-WebRequest "$Hub/api/connect/folders?device=$me" -UseBasicParsing).Content
      if ($body -is [byte[]]) { $body = [Text.Encoding]::UTF8.GetString($body) }
      $lines = @($body -split "`n" | Where-Object { $_.Trim() })
    } catch { $lines = @() }
    if ($lines.Count -gt 0) { break }
    if ($n -ge 90) {
      Write-Host "The box hasn't taken this computer yet. It may not be taking new devices"
      Write-Host 'today: ask whoever looks after it, then run this again.'
      Write-Host "This computer's Syncthing ID: $me"
      return
    }
    Start-Sleep 2
  }
  $have = @(& $st cli config folders list)
  $mine = $null
  foreach ($line in $lines) {
    $id, $label, $rel = $line.TrimEnd("`r") -split "`t"
    if ($have -contains $id) {
      if (-not (& $st cli config folders $id devices list | Where-Object { $_ -eq $BoxID })) {
        # The same ID from somewhere else, such as another box.
        Write-Host "Not adding ${label}: this computer has a folder with the same ID ($id),"
        Write-Host 'kept in step with another device. Ask whoever looks after the box.'
        continue
      }
      $path = (& $st cli config folders $id path get)
    } else {
      $path = Join-Path $env:USERPROFILE ($rel -replace '/', '\')
      # Not into a folder of yours that's already in use.
      if ($rel -eq 'yggstore' -and (Test-Path $path) -and (Get-ChildItem $path -Force | Select-Object -First 1)) {
        $path = Join-Path $env:USERPROFILE "yggstore-$Box"
      }
      New-Item -ItemType Directory $path -Force | Out-Null
      & $st cli config folders add --id $id --label $label --path $path
      & $st cli config folders $id devices add --device-id $BoxID
      Write-Host "Added ${label}: $path"
    }
    if ($id -ne 'yggstore-websites' -and -not $id.StartsWith('yggstore-team-')) { $mine = $path }
  }

  Write-Host ''
  Write-Host "Done. This computer is connected to the box $Box." -ForegroundColor Green
  if ($mine) {
    Write-Host "Your folder is $mine. Put files in its files folder to keep them safe;"
    Write-Host 'each becomes a small .ystub file. Copy a .ystub into restore to get it back.'
    Start-Process explorer.exe $mine
  }
  Write-Host "This computer's Syncthing ID (for Work from anywhere): $me"
}
