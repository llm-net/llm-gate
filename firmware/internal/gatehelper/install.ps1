param(
  [Parameter(Mandatory=$true)][string]$BaseUrl,
  [string]$ApiKey = ""
)
$ErrorActionPreference = 'Stop'
$BaseUrl = $BaseUrl.TrimEnd('/')
$runtimeArch = $null
try {
  $runtimeArch = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture
} catch {
  # Windows PowerShell 5.1 may not expose RuntimeInformation.OSArchitecture.
}
if ($null -ne $runtimeArch) {
  $arch = $runtimeArch.ToString().ToLowerInvariant()
} else {
  $arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
  $arch = "$arch".Trim().ToLowerInvariant()
}
switch ($arch) {
  'x64' { $goarch = 'amd64' }
  'amd64' { $goarch = 'amd64' }
  'arm64' { $goarch = 'arm64' }
  'aarch64' { $goarch = 'arm64' }
  default { throw "不支持的架构：$arch" }
}
$name = "gate-windows-$goarch.zip"
$temp = Join-Path ([IO.Path]::GetTempPath()) ("gate-install-" + [Guid]::NewGuid().ToString('N'))
$newDefault = Join-Path $env:LOCALAPPDATA 'Programs\LLMGate'
$legacyDefault = Join-Path $env:LOCALAPPDATA 'Programs\SoCAgent'
$installDir = if ($env:GATE_INSTALL_DIR) { $env:GATE_INSTALL_DIR } elseif ((Test-Path -LiteralPath (Join-Path $legacyDefault 'gate.exe')) -and -not (Test-Path -LiteralPath (Join-Path $newDefault 'gate.exe'))) { $legacyDefault } else { $newDefault }
$installDir = [IO.Path]::GetFullPath($installDir)
$configRoot = if ($env:GATE_CONFIG_DIR) { $env:GATE_CONFIG_DIR } else { Join-Path $env:APPDATA 'gate' }
# The downloaded gate holds the operation and usage leases while this fixed
# commit script replaces files and records ownership. No credential enters it.
$commitScript = @'
$ErrorActionPreference = 'Stop'
$BaseUrl = $env:GATE_INSTALL_BASE
$ApiKey = $env:GATE_INSTALL_KEY
$temp = $env:GATE_INSTALL_STAGE
$installDir = $env:GATE_INSTALL_TARGET_DIR
$configRoot = $env:GATE_INSTALL_CONFIG_ROOT
$source = Join-Path $temp 'gate.exe'
Remove-Item Env:GATE_INSTALL_KEY -ErrorAction SilentlyContinue
$configFile = Join-Path $configRoot 'config.json'
$stateFile = Join-Path $configRoot 'install-state.json'
$stateExisted = Test-Path -LiteralPath $stateFile
$target = Join-Path $installDir 'gate.exe'
$locatorFile = $target + '.install.json'
$locatorExisted = Test-Path -LiteralPath $locatorFile
$targetExisted = Test-Path -LiteralPath $target
$configExisted = Test-Path -LiteralPath $configFile
$targetReplaced = $false
$configCommitted = $false
$configTouched = $false
$pathChanged = $false
$pathUnavailable = $false
$previousUserPath = $null
function Move-Atomically([string]$Source, [string]$Destination) {
  if (Test-Path -LiteralPath $Destination) {
    # 备份名必须是 [NullString]::Value：PowerShell 把 $null 交给 .NET 的 string
    # 参数会变成空串，Windows 上的 .NET Framework 对空备份路径抛「路径的形式不合法」。
    [IO.File]::Replace($Source, $Destination, [NullString]::Value)
  } else {
    [IO.File]::Move($Source, $Destination)
  }
}
$env:GATE_INSTALL_DIR_CREATED = if (Test-Path -LiteralPath $installDir) { '0' } else { '1' }
New-Item -ItemType Directory -Force -Path $installDir | Out-Null
try {
  if ($targetExisted) { Copy-Item -LiteralPath $target -Destination (Join-Path $temp 'gate.previous') }
  if ($configExisted) { Copy-Item -LiteralPath $configFile -Destination (Join-Path $temp 'config.previous') }
  if ($stateExisted) { Copy-Item -LiteralPath $stateFile -Destination (Join-Path $temp 'state.previous') }
  if ($locatorExisted) { Copy-Item -LiteralPath $locatorFile -Destination (Join-Path $temp 'locator.previous') }
  $newTarget = Join-Path $installDir 'gate.exe.new'
  Copy-Item -Force $source $newTarget
  Move-Atomically $newTarget $target
  $targetReplaced = $true

  $configTouched = $true
  if ($ApiKey) {
    & $target bootstrap $BaseUrl $ApiKey
  } else {
    & $target bootstrap $BaseUrl
  }
  if ($LASTEXITCODE -ne 0) { throw "gate 初始化失败（退出码 $LASTEXITCODE）：常见原因是设备地址或 API Key 验证失败，确切原因见上方 gate 输出" }
  & $target __record-install
  if ($LASTEXITCODE -ne 0) { throw '安装归属记录写入失败' }
  $configCommitted = $true

  # 用户 PATH 读写可能被权限或策略阻止，不应撤销已经成功的程序与配置安装。
  # 当前会话 PATH 由外层脚本在提交成功后补上；归属记录只记实际写入的用户 PATH。
  try {
    $previousUserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $parts = @($previousUserPath -split ';' | Where-Object { $_ })
    if (-not ($parts | Where-Object { $_.TrimEnd('\') -ieq $installDir.TrimEnd('\') })) {
      [Environment]::SetEnvironmentVariable('Path', $(if ([string]::IsNullOrEmpty($previousUserPath)) { $installDir } else { $previousUserPath + ';' + $installDir }), 'User')
      $pathChanged = $true
    }
  } catch {
    $pathUnavailable = $true
  }
  if ($pathChanged) {
    # 写入成功后的归属记录失败仍属提交失败，必须连同 PATH 一起回滚。
    & $target __record-install --user-path $installDir
    if ($LASTEXITCODE -ne 0) { throw '用户 PATH 归属记录写入失败' }
  }
  Write-Host "gate 已安装到 $target"
  if ($pathUnavailable) {
    Write-Host "无法更新用户 PATH，程序与配置已安装。请在 Windows 环境变量设置中把 $installDir 加入用户 Path；其他终端也可通过完整路径运行 gate.exe。"
  } elseif ($pathChanged) {
    Write-Host "已把 $installDir 写入用户 PATH；请重新打开其他终端。"
  }
} catch {
  $cause = $_
  $rollbackErrors = @()
  if ($targetReplaced) {
    try {
      if ($targetExisted) {
        $restore = Join-Path $installDir 'gate.exe.rollback'
        Copy-Item -Force (Join-Path $temp 'gate.previous') $restore
        Move-Atomically $restore $target
      } else {
        Remove-Item -Force -ErrorAction Stop $target
      }
    } catch { $rollbackErrors += "程序：$($_.Exception.Message)" }
  }
  if ($configTouched -or $configCommitted) {
    try {
      if ($configExisted) {
        New-Item -ItemType Directory -Force -Path $configRoot | Out-Null
        $restore = "$configFile.rollback"
        Copy-Item -Force (Join-Path $temp 'config.previous') $restore
        Move-Atomically $restore $configFile
      } elseif (Test-Path -LiteralPath $configFile) {
        Remove-Item -Force -ErrorAction Stop $configFile
      }
    } catch { $rollbackErrors += "配置：$($_.Exception.Message)" }
  }
  if ($configTouched -or $configCommitted) {
    try {
      if ($stateExisted) {
        $restoreState = "$stateFile.rollback"
        Copy-Item -Force (Join-Path $temp 'state.previous') $restoreState
        Move-Atomically $restoreState $stateFile
      } elseif (Test-Path -LiteralPath $stateFile) { Remove-Item -LiteralPath $stateFile -Force }
    } catch { $rollbackErrors += "归属记录：$($_.Exception.Message)" }
  }
  if ($configTouched -or $configCommitted) {
    try {
      if ($locatorExisted) {
        $restoreLocator = "$locatorFile.rollback"
        Copy-Item -Force (Join-Path $temp 'locator.previous') $restoreLocator
        Move-Atomically $restoreLocator $locatorFile
      } elseif (Test-Path -LiteralPath $locatorFile) { Remove-Item -LiteralPath $locatorFile -Force }
    } catch { $rollbackErrors += "定位记录：$($_.Exception.Message)" }
  }
  if ($pathChanged) {
    try { [Environment]::SetEnvironmentVariable('Path', $previousUserPath, 'User') }
    catch { $rollbackErrors += "PATH：$($_.Exception.Message)" }
  }
  if ($rollbackErrors.Count -gt 0) {
    throw "安装失败：$($cause.Exception.Message)；自动回退未完整成功：$($rollbackErrors -join '；')"
  }
  throw "安装失败：$($cause.Exception.Message)；原程序与配置已恢复"
}

'@
New-Item -ItemType Directory -Force -Path $temp | Out-Null
$forwarded = @('GATE_INSTALL_BASE','GATE_INSTALL_KEY','GATE_INSTALL_STAGE','GATE_INSTALL_TARGET_DIR','GATE_INSTALL_CONFIG_ROOT')
$previousForwarded = @{}
foreach ($keyName in $forwarded) { $previousForwarded[$keyName] = [Environment]::GetEnvironmentVariable($keyName,'Process') }
try {
  Invoke-WebRequest -UseBasicParsing "$BaseUrl/gate-helper/releases/$name" -OutFile (Join-Path $temp $name)
  $manifest = Invoke-RestMethod -Method Get "$BaseUrl/gate-helper/releases/stable.json"
  $entry = $manifest.assets | Where-Object { $_.name -eq $name } | Select-Object -First 1
  if (-not $entry) { throw '校验清单没有当前平台' }
  $got = (Get-FileHash -Algorithm SHA256 (Join-Path $temp $name)).Hash.ToLowerInvariant()
  if ($got -ne $entry.sha256.ToLowerInvariant()) { throw 'gate 压缩包 SHA-256 不匹配' }
  Expand-Archive -Force (Join-Path $temp $name) $temp
  $source = Join-Path $temp 'gate.exe'
  if (-not (Test-Path $source)) { throw '压缩包缺少 gate.exe' }
  $commitFile = Join-Path $temp 'commit.ps1'
  [IO.File]::WriteAllText($commitFile, $commitScript, (New-Object Text.UTF8Encoding($true)))
  $env:GATE_INSTALL_BASE = $BaseUrl
  $env:GATE_INSTALL_KEY = $ApiKey
  $env:GATE_INSTALL_STAGE = $temp
  $env:GATE_INSTALL_TARGET_DIR = $installDir
  $env:GATE_INSTALL_CONFIG_ROOT = $configRoot
  $hostProgram = [Diagnostics.Process]::GetCurrentProcess().MainModule.FileName
  & $source __installer-exec $hostProgram -NoLogo -NoProfile -NonInteractive -File $commitFile
  if ($LASTEXITCODE -ne 0) { throw 'gate 安装提交失败，确切原因见上方输出；已尝试恢复原状态' }
  $sessionParts = @("$env:Path" -split ';')
  if (-not ($sessionParts | Where-Object { $_.TrimEnd('\') -ieq $installDir.TrimEnd('\') })) {
    $env:Path = if ([string]::IsNullOrEmpty($env:Path)) { $installDir } else { $env:Path + ';' + $installDir }
    Write-Host "$installDir 已加入当前 PowerShell 会话 PATH。"
  }
} finally {
  foreach ($keyName in $forwarded) { [Environment]::SetEnvironmentVariable($keyName,$previousForwarded[$keyName],'Process') }
  Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $temp
}
