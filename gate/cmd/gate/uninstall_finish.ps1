param([Parameter(Mandatory=$true)][string]$Job, [switch]$Retry)
$ErrorActionPreference = 'Stop'
$taskDir = [IO.Path]::GetDirectoryName([IO.Path]::GetFullPath($Job))
$resultPath = Join-Path $taskDir 'result.txt'
try {
  $jobData = Get-Content -LiteralPath $Job -Raw -Encoding UTF8 | ConvertFrom-Json
  if ($jobData.schema -ne 1 -or -not [IO.Path]::IsPathRooted($jobData.target) -or $jobData.sha256 -notmatch '^[a-f0-9]{64}$' -or $jobData.identity -notmatch '^[a-f0-9]{8}-[a-f0-9]{8}-[a-f0-9]{8}$') { throw 'Invalid uninstall job' }
  Add-Type -TypeDefinition @'
using System;
using System.IO;
using System.Runtime.InteropServices;
using System.Security.Cryptography;
using Microsoft.Win32.SafeHandles;
public static class GateRemoval {
  [StructLayout(LayoutKind.Sequential)] struct Info {
    public uint Attributes; public System.Runtime.InteropServices.ComTypes.FILETIME Creation,Access,Write;
    public uint Volume,SizeHigh,SizeLow,Links,IndexHigh,IndexLow;
  }
  [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern SafeFileHandle CreateFile(string path,uint access,uint share,IntPtr security,uint creation,uint flags,IntPtr template);
  [DllImport("kernel32.dll", SetLastError=true)] static extern bool GetFileInformationByHandle(SafeFileHandle handle,out Info info);
  [DllImport("kernel32.dll", SetLastError=true)] static extern bool SetFileInformationByHandle(SafeFileHandle handle,int type,ref byte data,uint size);
  public static void Delete(string path,string identity,string hash) {
    // DELETE + GENERIC_READ, share read only: a reinstallation cannot replace
    // this file between fingerprint verification and deletion by handle.
    using(var handle=CreateFile(path,0x80010000,1,IntPtr.Zero,3,0x00200000,IntPtr.Zero)) {
      if(handle.IsInvalid) throw new IOException("Cannot open target for removal: "+Marshal.GetLastWin32Error());
      Info info; if(!GetFileInformationByHandle(handle,out info)) throw new IOException("Cannot identify target");
      if((info.Attributes&0x400)!=0) throw new IOException("Reparse target rejected");
      string actual=info.Volume.ToString("x8")+"-"+info.IndexHigh.ToString("x8")+"-"+info.IndexLow.ToString("x8");
      if(actual!=identity) throw new IOException("Target has been replaced; new installation preserved");
      using(var stream=new FileStream(handle,FileAccess.Read)) {
        using(var sha=SHA256.Create()) {
          string got=BitConverter.ToString(sha.ComputeHash(stream)).Replace("-","").ToLowerInvariant();
          if(got!=hash) throw new IOException("Target contents changed; file preserved");
        }
        byte remove=1;
        if(!SetFileInformationByHandle(handle,4,ref remove,1)) throw new IOException("Cannot remove target: "+Marshal.GetLastWin32Error());
      }
    }
  }
}
'@
  if (-not $Retry) {
    $parent = [Diagnostics.Process]::GetProcessById([int]$jobData.pid)
    $null = $parent.Handle # bind to this process instance before signalling ready
    if ($parent.StartTime.ToUniversalTime().ToFileTimeUtc().ToString() -ne $jobData.started) { throw 'Parent process identity changed' }
    [IO.File]::WriteAllText((Join-Path $taskDir 'ready'), 'ready')
    while (-not $parent.WaitForExit(200)) {
      if (Test-Path -LiteralPath (Join-Path $taskDir 'cancel')) { break }
    }
    $parent.Dispose()
  }
  if ((Test-Path -LiteralPath (Join-Path $taskDir 'commit')) -and -not (Test-Path -LiteralPath (Join-Path $taskDir 'cancel'))) {
    # Reject linked/junction parents as well as a linked leaf.
    $node = [IO.Directory]::GetParent($jobData.target)
    while ($null -ne $node) {
      if (($node.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'Parent reparse point rejected' }
      $node = $node.Parent
    }
    if (Test-Path -LiteralPath $jobData.target) { [GateRemoval]::Delete($jobData.target,$jobData.identity,$jobData.sha256) }
    if ($jobData.cleanup_directory) {
      if ([IO.Path]::GetFullPath($jobData.cleanup_directory) -ne [IO.Path]::GetDirectoryName([IO.Path]::GetFullPath($jobData.target))) { throw 'Cleanup directory does not match target' }
      if ([IO.Directory]::GetFileSystemEntries($jobData.cleanup_directory).Length -eq 0) { [IO.Directory]::Delete($jobData.cleanup_directory, $false) }
    }
    [IO.File]::WriteAllText($resultPath, 'status=complete', [Text.Encoding]::UTF8)
    Write-Output 'gate 程序删除完成。' 
  }
  foreach ($name in @('ready','commit','cancel','job.json','finish.ps1')) {
    $file = Join-Path $taskDir $name
    if (Test-Path -LiteralPath $file) { Remove-Item -LiteralPath $file -Force }
  }
  if (-not $jobData.keep_result) {
    if (Test-Path -LiteralPath $resultPath) { Remove-Item -LiteralPath $resultPath -Force }
    [IO.Directory]::Delete($taskDir, $false)
  }
} catch {
  $message = 'gate 自卸载收尾失败，程序或结果文件已保留。请关闭占用程序后重试系统 PowerShell：' + [Environment]::NewLine +
    'powershell.exe -NoProfile -File "' + (Join-Path $taskDir 'finish.ps1') + '" -Job "' + $Job + '" -Retry' + [Environment]::NewLine + $_.Exception.Message
  [IO.File]::WriteAllText($resultPath, $message, [Text.Encoding]::UTF8)
  Write-Error $message
  exit 1
}
