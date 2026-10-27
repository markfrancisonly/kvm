#Requires -Version 5.1
<#
.SYNOPSIS
  JetKVM host agent: pastes text, screenshots and files sent through the JetKVM
  into the window that has the focus on this computer.

.DESCRIPTION
  Runs as the signed-in user, needs no installation and no admin rights. It
  talks to the JetKVM over the "USB Serial Console" port of the JetKVM's USB
  device (enable it under Settings > Hardware > USB devices on the JetKVM).
  Text arrives as Unicode key events, so any character works regardless of the
  keyboard layout. Images and files are placed on the clipboard and pasted.

  Start it with:
    powershell -ExecutionPolicy Bypass -File jetkvm-host-agent.ps1

  -Install adds a shortcut to your Startup folder so it runs at sign-in,
  -Uninstall removes it again.

.PARAMETER Port
  COM port to use. Found automatically from the JetKVM USB identifiers when
  omitted.
.PARAMETER DevicePath
  A serial device path to open as a plain file instead of a COM port, for
  example /dev/ttyACM0 under PowerShell 7 on Linux. Text is only typed on
  Windows; elsewhere the agent prints what it would have typed.
.PARAMETER VendorId
.PARAMETER ProductId
  USB identifiers of the JetKVM, as hex without 0x. Defaults match a stock
  device; change them if you customised them on the JetKVM.
.PARAMETER SaveDir
  Where pasted files are stored before they are placed on the clipboard.
.PARAMETER TypeDelayMs
  Pause between batches of 64 typed characters, for applications that drop
  fast input. 0 by default.
#>
[CmdletBinding()]
param(
  [string]$Port,
  [string]$DevicePath,
  [string]$VendorId = "1D6B",
  [string]$ProductId = "0104",
  [string]$SaveDir = (Join-Path ([Environment]::GetFolderPath("LocalApplicationData")) "JetKVM\paste"),
  [int]$TypeDelayMs = 0,
  [switch]$Install,
  [switch]$Uninstall
)

Set-StrictMode -Version 2
$ErrorActionPreference = "Stop"

$script:AgentVersion = "0.1.0"
$script:IsWindowsHost = ($PSVersionTable.PSVersion.Major -lt 6) -or $IsWindows

# ---------------------------------------------------------------- startup shortcut

function Get-StartupShortcutPath {
  Join-Path ([Environment]::GetFolderPath("Startup")) "JetKVM host agent.lnk"
}

if ($Install) {
  $shell = New-Object -ComObject WScript.Shell
  $lnk = $shell.CreateShortcut((Get-StartupShortcutPath))
  $lnk.TargetPath = (Get-Command powershell.exe).Source
  $lnk.Arguments = "-NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File `"$PSCommandPath`""
  $lnk.WorkingDirectory = Split-Path $PSCommandPath
  $lnk.Description = "JetKVM host agent"
  $lnk.Save()
  Write-Host "Startup shortcut created: $(Get-StartupShortcutPath)"
  return
}
if ($Uninstall) {
  $p = Get-StartupShortcutPath
  if (Test-Path $p) { Remove-Item $p; Write-Host "Removed $p" } else { Write-Host "No startup shortcut found" }
  return
}

# ---------------------------------------------------------------- logging

function Log([string]$msg) {
  Write-Host ("{0:HH:mm:ss} {1}" -f (Get-Date), $msg)
}

# ---------------------------------------------------------------- frame codec
# Frame: "JKVM" | version u8=1 | type u8 | flags u8 | reserved u8 | seq u32 | length u32 | payload | crc32
# Little-endian. CRC-32 (IEEE) covers everything before it.

$script:Magic = [byte[]](0x4A, 0x4B, 0x56, 0x4D)
$script:HeaderSize = 16
$script:MaxPayload = 65536

$script:T = @{
  Hello = 0x01; Ack = 0x02; Nack = 0x03; Ping = 0x04; Pong = 0x05; Bye = 0x06
  Text = 0x10
  BlobBegin = 0x20; BlobData = 0x21; BlobEnd = 0x22; BlobCancel = 0x23
  Status = 0x30
}
$script:FlagTextClipboard = 0x01

# CRC in C#: PowerShell loops are too slow for 16 KiB blob chunks, and its
# hex literals are signed 32-bit, which makes the usual table code fail.
Add-Type -TypeDefinition @"
public static class JetKvmCrc32 {
  static readonly uint[] Table = MakeTable();
  static uint[] MakeTable() {
    uint[] t = new uint[256];
    for (uint i = 0; i < 256; i++) {
      uint c = i;
      for (int k = 0; k < 8; k++) c = (c & 1) != 0 ? 0xEDB88320u ^ (c >> 1) : c >> 1;
      t[i] = c;
    }
    return t;
  }
  public static uint Compute(byte[] data, int count) {
    uint crc = 0xFFFFFFFFu;
    for (int i = 0; i < count; i++) crc = Table[(crc ^ data[i]) & 0xFF] ^ (crc >> 8);
    return crc ^ 0xFFFFFFFFu;
  }
}
"@

function Get-Crc32([byte[]]$data, [int]$count) {
  return [JetKvmCrc32]::Compute($data, $count)
}

function New-Frame([int]$type, [uint32]$seq, [byte[]]$payload, [int]$flags = 0) {
  if ($null -eq $payload) { $payload = [byte[]]@() }
  $n = $script:HeaderSize + $payload.Length + 4
  $b = New-Object byte[] $n
  [Array]::Copy($script:Magic, 0, $b, 0, 4)
  $b[4] = 1; $b[5] = [byte]$type; $b[6] = [byte]$flags; $b[7] = 0
  [Array]::Copy([BitConverter]::GetBytes($seq), 0, $b, 8, 4)
  [Array]::Copy([BitConverter]::GetBytes([uint32]$payload.Length), 0, $b, 12, 4)
  if ($payload.Length -gt 0) { [Array]::Copy($payload, 0, $b, $script:HeaderSize, $payload.Length) }
  $crc = Get-Crc32 $b ($n - 4)
  [Array]::Copy([BitConverter]::GetBytes($crc), 0, $b, $n - 4, 4)
  return ,$b
}

function New-AckPayload([uint32]$seq, [string]$note) {
  $noteBytes = [Text.Encoding]::UTF8.GetBytes($note)
  $p = New-Object byte[] (4 + $noteBytes.Length)
  [Array]::Copy([BitConverter]::GetBytes($seq), 0, $p, 0, 4)
  if ($noteBytes.Length -gt 0) { [Array]::Copy($noteBytes, 0, $p, 4, $noteBytes.Length) }
  return ,$p
}

# Decoder state: a growing buffer that is scanned for the next valid frame.
$script:RxBuf = New-Object System.Collections.Generic.List[byte]

function Find-Magic([System.Collections.Generic.List[byte]]$buf, [int]$from) {
  for ($i = $from; $i -le $buf.Count - 4; $i++) {
    if ($buf[$i] -eq 0x4A -and $buf[$i + 1] -eq 0x4B -and $buf[$i + 2] -eq 0x56 -and $buf[$i + 3] -eq 0x4D) { return $i }
  }
  return -1
}

# Returns the next complete frame as a hashtable, or $null when more bytes are needed.
function Read-NextFrame {
  while ($true) {
    $i = Find-Magic $script:RxBuf 0
    if ($i -lt 0) {
      if ($script:RxBuf.Count -gt 3) { $script:RxBuf.RemoveRange(0, $script:RxBuf.Count - 3) }
      return $null
    }
    if ($i -gt 0) { $script:RxBuf.RemoveRange(0, $i) }
    if ($script:RxBuf.Count -lt $script:HeaderSize) { return $null }
    $hdr = $script:RxBuf.GetRange(0, $script:HeaderSize).ToArray()
    $len = [BitConverter]::ToUInt32($hdr, 12)
    $ok = ($hdr[4] -eq 1) -and ($len -le $script:MaxPayload)
    if ($ok) {
      $n = $script:HeaderSize + [int]$len + 4
      if ($script:RxBuf.Count -lt $n) { return $null }
      $raw = $script:RxBuf.GetRange(0, $n).ToArray()
      $want = [BitConverter]::ToUInt32($raw, $n - 4)
      if ((Get-Crc32 $raw ($n - 4)) -eq $want) {
        $script:RxBuf.RemoveRange(0, $n)
        $payload = New-Object byte[] $len
        if ($len -gt 0) { [Array]::Copy($raw, $script:HeaderSize, $payload, 0, $len) }
        return @{
          Type = [int]$raw[5]; Flags = [int]$raw[6]
          Seq = [BitConverter]::ToUInt32($raw, 8); Payload = $payload
        }
      }
    }
    # Not a frame after all: resume the search after this magic.
    $script:RxBuf.RemoveAt(0)
  }
}

# ---------------------------------------------------------------- transport

$script:Link = $null      # SerialPort or FileStream
$script:LinkIsPort = $false

function Find-JetKvmPorts {
  $vid = $VendorId.ToUpper(); $pid = $ProductId.ToUpper()
  $ports = @()
  try {
    $devs = Get-CimInstance Win32_PnPEntity -Filter "PNPClass='Ports'" -ErrorAction Stop
  } catch {
    $devs = Get-WmiObject Win32_PnPEntity | Where-Object { $_.PNPClass -eq "Ports" }
  }
  foreach ($d in $devs) {
    if ($d.PNPDeviceID -notlike "USB\VID_$vid&PID_$pid*") { continue }
    if ($d.Name -match "\((COM\d+)\)") { $ports += $Matches[1] }
  }
  return $ports
}

function Open-Link {
  if ($DevicePath) {
    $fs = New-Object System.IO.FileStream($DevicePath, [IO.FileMode]::Open, [IO.FileAccess]::ReadWrite, [IO.FileShare]::ReadWrite, 4096, $false)
    $script:Link = $fs; $script:LinkIsPort = $false
    Log "Opened $DevicePath"
    return $true
  }
  $candidates = if ($Port) { @($Port) } else { Find-JetKvmPorts }
  if ($candidates.Count -eq 0) { return $false }
  foreach ($c in $candidates) {
    try {
      $sp = New-Object System.IO.Ports.SerialPort $c, 115200, "None", 8, "One"
      $sp.ReadTimeout = 200
      $sp.WriteTimeout = 2000
      $sp.ReadBufferSize = 1048576
      $sp.WriteBufferSize = 262144
      $sp.DtrEnable = $true
      $sp.RtsEnable = $true
      $sp.Open()
      $script:Link = $sp; $script:LinkIsPort = $true
      Log "Opened $c"
      return $true
    } catch {
      Log "Cannot open ${c}: $($_.Exception.Message)"
    }
  }
  return $false
}

function Close-Link {
  if ($null -ne $script:Link) {
    try { $script:Link.Close() } catch { }
    try { $script:Link.Dispose() } catch { }
  }
  $script:Link = $null
  $script:RxBuf.Clear()
}

function Write-Link([byte[]]$bytes) {
  if ($script:LinkIsPort) { $script:Link.Write($bytes, 0, $bytes.Length) }
  else { $script:Link.Write($bytes, 0, $bytes.Length); $script:Link.Flush() }
}

$script:ReadScratch = New-Object byte[] 65536

# Reads whatever is available (blocking up to ~200 ms) into the decoder buffer.
function Read-Link {
  if ($script:LinkIsPort) {
    try {
      $n = $script:Link.Read($script:ReadScratch, 0, $script:ReadScratch.Length)
    } catch [System.TimeoutException] {
      return
    }
  } else {
    # FileStream on a tty blocks until at least one byte arrives.
    $n = $script:Link.Read($script:ReadScratch, 0, $script:ReadScratch.Length)
    if ($n -le 0) { throw "device closed" }
  }
  if ($n -gt 0) { $script:RxBuf.AddRange([byte[]]$script:ReadScratch[0..($n - 1)]) }
}

$script:Seq = [uint32]0
function Send-Frame([int]$type, [byte[]]$payload, [int]$flags = 0) {
  $script:Seq++
  Write-Link (New-Frame $type $script:Seq $payload $flags)
  return $script:Seq
}
function Send-Ack([uint32]$seq, [string]$note = "") { [void](Send-Frame $script:T.Ack (New-AckPayload $seq $note)) }
function Send-Nack([uint32]$seq, [string]$reason) { [void](Send-Frame $script:T.Nack (New-AckPayload $seq $reason)) }

# ---------------------------------------------------------------- input injection (Windows)

if ($script:IsWindowsHost) {
  Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
using System.Text;

public static class JetKvmInput {
  [StructLayout(LayoutKind.Sequential)]
  struct KEYBDINPUT { public ushort wVk; public ushort wScan; public uint dwFlags; public uint time; public IntPtr dwExtraInfo; }
  [StructLayout(LayoutKind.Explicit)]
  struct INPUTUNION { [FieldOffset(0)] public KEYBDINPUT ki; [FieldOffset(0)] public long pad1; [FieldOffset(8)] public long pad2; [FieldOffset(16)] public long pad3; }
  [StructLayout(LayoutKind.Sequential)]
  struct INPUT { public uint type; public INPUTUNION u; }

  const uint INPUT_KEYBOARD = 1;
  const uint KEYEVENTF_KEYUP = 0x0002;
  const uint KEYEVENTF_UNICODE = 0x0004;
  const ushort VK_RETURN = 0x0D, VK_TAB = 0x09, VK_CONTROL = 0x11, VK_V = 0x56;

  [DllImport("user32.dll", SetLastError = true)]
  static extern uint SendInput(uint nInputs, INPUT[] pInputs, int cbSize);

  static INPUT Key(ushort vk, bool up) {
    INPUT i = new INPUT(); i.type = INPUT_KEYBOARD; i.u.ki.wVk = vk; i.u.ki.dwFlags = up ? KEYEVENTF_KEYUP : 0; return i;
  }
  static INPUT Unicode(char c, bool up) {
    INPUT i = new INPUT(); i.type = INPUT_KEYBOARD; i.u.ki.wScan = c; i.u.ki.dwFlags = KEYEVENTF_UNICODE | (up ? KEYEVENTF_KEYUP : 0); return i;
  }

  // Sends a batch; returns false when Windows refused it (typically the
  // focused window runs elevated and user interface privilege isolation
  // blocks input from this process).
  static bool Send(INPUT[] batch, int count) {
    if (count == 0) return true;
    INPUT[] exact = new INPUT[count]; Array.Copy(batch, exact, count);
    return SendInput((uint)count, exact, Marshal.SizeOf(typeof(INPUT))) == (uint)count;
  }

  // Types text as Unicode key events. Newlines become Enter, tabs become Tab.
  public static bool TypeText(string text, int batchDelayMs) {
    INPUT[] batch = new INPUT[128]; int n = 0;
    for (int i = 0; i < text.Length; i++) {
      char c = text[i];
      if (c == '\r') { if (i + 1 < text.Length && text[i + 1] == '\n') continue; c = '\n'; }
      if (c == '\n') { batch[n++] = Key(VK_RETURN, false); batch[n++] = Key(VK_RETURN, true); }
      else if (c == '\t') { batch[n++] = Key(VK_TAB, false); batch[n++] = Key(VK_TAB, true); }
      else { batch[n++] = Unicode(c, false); batch[n++] = Unicode(c, true); }
      if (n >= 128) {
        if (!Send(batch, n)) return false;
        n = 0;
        if (batchDelayMs > 0) System.Threading.Thread.Sleep(batchDelayMs);
      }
    }
    return Send(batch, n);
  }

  public static bool PressCtrlV() {
    INPUT[] b = new INPUT[4];
    b[0] = Key(VK_CONTROL, false); b[1] = Key(VK_V, false); b[2] = Key(VK_V, true); b[3] = Key(VK_CONTROL, true);
    return Send(b, 4);
  }
}
"@
  Add-Type -AssemblyName System.Windows.Forms
  Add-Type -AssemblyName System.Drawing
}

# Clipboard calls need a single-threaded apartment.
function Invoke-Sta([scriptblock]$block) {
  if ([Threading.Thread]::CurrentThread.GetApartmentState() -eq "STA") { return & $block }
  $result = $null; $err = $null
  $thread = New-Object Threading.Thread([Threading.ThreadStart] {
      try { $script:StaResult = & $block } catch { $script:StaError = $_ }
    })
  $thread.SetApartmentState("STA"); $thread.Start(); $thread.Join()
  if ($script:StaError) { $e = $script:StaError; $script:StaError = $null; throw $e }
  $result = $script:StaResult; $script:StaResult = $null
  return $result
}

function Invoke-TypeText([string]$text, [bool]$clipboardMode) {
  if (-not $script:IsWindowsHost) {
    Log ("TEXT ({0} chars, {1}): {2}" -f $text.Length, $(if ($clipboardMode) { "clipboard" } else { "type" }), $text)
    return $true
  }
  if (-not $clipboardMode) {
    return [JetKvmInput]::TypeText($text, $TypeDelayMs)
  }
  return Invoke-Sta {
    $previous = $null
    if ([Windows.Forms.Clipboard]::ContainsText()) { $previous = [Windows.Forms.Clipboard]::GetText() }
    [Windows.Forms.Clipboard]::SetText($text)
    $ok = [JetKvmInput]::PressCtrlV()
    Start-Sleep -Milliseconds 500
    if ($null -ne $previous) { [Windows.Forms.Clipboard]::SetText($previous) }
    return $ok
  }
}

# ---------------------------------------------------------------- blobs

$script:Blob = $null   # @{ Id; Kind; Name; Mime; Size; Paste; Path; Stream; Written }

function Get-SafeFileName([string]$name) {
  $n = [IO.Path]::GetFileName($name)
  foreach ($c in [IO.Path]::GetInvalidFileNameChars()) { $n = $n.Replace($c, "_") }
  if ([string]::IsNullOrWhiteSpace($n)) { $n = "paste.bin" }
  return $n
}

function Start-Blob($frame) {
  $meta = [Text.Encoding]::UTF8.GetString($frame.Payload) | ConvertFrom-Json
  Stop-Blob -Delete $true
  $dir = Join-Path $SaveDir (Get-Date -Format "yyyyMMdd-HHmmss")
  New-Item -ItemType Directory -Force -Path $dir | Out-Null
  $path = Join-Path $dir (Get-SafeFileName $meta.name)
  $script:Blob = @{
    Id = [uint32]$meta.id; Kind = [string]$meta.kind; Name = [string]$meta.name; Mime = [string]$meta.mime
    Size = [long]$meta.size; Paste = [bool]$meta.paste; Path = $path; Written = [long]0
    Stream = [IO.File]::Open($path, [IO.FileMode]::Create, [IO.FileAccess]::Write)
  }
  Log ("Receiving {0} {1} ({2} bytes)" -f $script:Blob.Kind, $script:Blob.Name, $script:Blob.Size)
}

function Stop-Blob([bool]$Delete) {
  if ($null -eq $script:Blob) { return }
  try { $script:Blob.Stream.Close() } catch { }
  if ($Delete) { try { Remove-Item $script:Blob.Path -Force } catch { } }
  $script:Blob = $null
}

function Add-BlobData($frame) {
  if ($null -eq $script:Blob) { return "no_blob" }
  $p = $frame.Payload
  if ($p.Length -lt 8) { return "bad_frame" }
  $id = [BitConverter]::ToUInt32($p, 0)
  $offset = [BitConverter]::ToUInt32($p, 4)
  if ($id -ne $script:Blob.Id) { return "bad_blob" }
  if ($offset -ne $script:Blob.Written) { return "bad_offset" }
  $count = $p.Length - 8
  if ($script:Blob.Written + $count -gt $script:Blob.Size) { return "too_large" }
  $script:Blob.Stream.Write($p, 8, $count)
  $script:Blob.Written += $count
  return ""
}

function Complete-Blob {
  if ($null -eq $script:Blob) { return "no_blob" }
  $b = $script:Blob
  $b.Stream.Close()
  if ($b.Written -ne $b.Size) { Stop-Blob -Delete $true; return "incomplete" }
  $script:Blob = $null
  Log ("Saved {0}" -f $b.Path)
  if (-not $script:IsWindowsHost) { return "" }
  $ok = Invoke-Sta {
    if ($b.Kind -eq "image") {
      $img = [Drawing.Image]::FromFile($b.Path)
      try { [Windows.Forms.Clipboard]::SetImage($img) } finally { $img.Dispose() }
    } else {
      $list = New-Object Collections.Specialized.StringCollection
      [void]$list.Add($b.Path)
      [Windows.Forms.Clipboard]::SetFileDropList($list)
    }
    if ($b.Paste) { return [JetKvmInput]::PressCtrlV() }
    return $true
  }
  if (-not $ok) { return "elevated" }
  return ""
}

function Remove-OldPastes {
  if (-not (Test-Path $SaveDir)) { return }
  Get-ChildItem $SaveDir -Directory | Where-Object { $_.LastWriteTime -lt (Get-Date).AddDays(-7) } |
    ForEach-Object { try { Remove-Item $_.FullName -Recurse -Force } catch { } }
}

# ---------------------------------------------------------------- protocol session

$script:HelloPayload = [Text.Encoding]::UTF8.GetBytes((@{
      v = 1
      os = $(if ($script:IsWindowsHost) { "windows" } elseif ($IsMacOS) { "macos" } else { "linux" })
      agent = "jetkvm-host-agent"
      version = $script:AgentVersion
      caps = @("text", "clipboard", "image", "files")
    } | ConvertTo-Json -Compress))

function Handle-Frame($f) {
  switch ($f.Type) {
    $script:T.Text {
      $text = [Text.Encoding]::UTF8.GetString($f.Payload)
      $clip = ($f.Flags -band $script:FlagTextClipboard) -ne 0
      $ok = $false
      try { $ok = Invoke-TypeText $text $clip } catch { Log "Typing failed: $($_.Exception.Message)" }
      if ($ok) { Send-Ack $f.Seq } else { Send-Nack $f.Seq "elevated" }
    }
    $script:T.Pong { }
    $script:T.Ping { [void](Send-Frame $script:T.Pong (New-AckPayload $f.Seq "")) }
    $script:T.Ack { }
    $script:T.Nack {
      $note = if ($f.Payload.Length -gt 4) { [Text.Encoding]::UTF8.GetString($f.Payload, 4, $f.Payload.Length - 4) } else { "" }
      Log "Device refused a frame: $note"
    }
    $script:T.BlobBegin {
      try { Start-Blob $f; Send-Ack $f.Seq } catch { Log "Blob begin failed: $($_.Exception.Message)"; Send-Nack $f.Seq "bad_frame" }
    }
    $script:T.BlobData {
      $err = Add-BlobData $f
      if ($err) { Stop-Blob -Delete $true; Send-Nack $f.Seq $err } else { Send-Ack $f.Seq }
    }
    $script:T.BlobEnd {
      $err = ""
      try { $err = Complete-Blob } catch { Log "Blob paste failed: $($_.Exception.Message)"; $err = "failed" }
      if ($err) { Send-Nack $f.Seq $err } else { Send-Ack $f.Seq }
    }
    $script:T.BlobCancel { Stop-Blob -Delete $true; Log "Transfer cancelled" }
    default { Log ("Ignoring frame type 0x{0:X2}" -f $f.Type) }
  }
}

function Run-Session {
  # Announce until the device answers, then keep the link alive with pings.
  $announced = $false
  $helloSeq = [uint32]0
  $lastHello = [DateTime]::MinValue
  $lastPing = Get-Date
  $lastHeard = Get-Date
  while ($true) {
    $now = Get-Date
    if (-not $announced -and ($now - $lastHello).TotalSeconds -ge 2) {
      $helloSeq = Send-Frame $script:T.Hello $script:HelloPayload
      $lastHello = $now
    }
    if ($announced -and ($now - $lastPing).TotalSeconds -ge 5) {
      [void](Send-Frame $script:T.Ping $null)
      $lastPing = $now
    }
    if ($announced -and ($now - $lastHeard).TotalSeconds -ge 15) {
      Log "No answer from the JetKVM for 15 s, reconnecting"
      return
    }
    Read-Link
    while ($true) {
      $f = Read-NextFrame
      if ($null -eq $f) { break }
      $lastHeard = Get-Date
      if (-not $announced) {
        if ($f.Type -eq $script:T.Ack -and $f.Payload.Length -ge 4 -and [BitConverter]::ToUInt32($f.Payload, 0) -eq $helloSeq) {
          $announced = $true
          $info = [Text.Encoding]::UTF8.GetString($f.Payload, 4, $f.Payload.Length - 4)
          Log "Connected to the JetKVM: $info"
        } elseif ($f.Type -eq $script:T.Nack) {
          Log "The JetKVM refused the announcement; is its firmware up to date?"
          Start-Sleep -Seconds 10
        }
        continue
      }
      Handle-Frame $f
    }
  }
}

# ---------------------------------------------------------------- main loop

Log "JetKVM host agent $($script:AgentVersion) starting (PowerShell $($PSVersionTable.PSVersion))"
if ($script:IsWindowsHost) { Remove-OldPastes }
$reported = $false
while ($true) {
  try {
    if (-not (Open-Link)) {
      if (-not $reported) {
        Log "No JetKVM serial port found (USB $VendorId`:$ProductId). Enable 'USB Serial Console' on the JetKVM; waiting."
        $reported = $true
      }
      Start-Sleep -Seconds 3
      continue
    }
    $reported = $false
    Run-Session
  } catch {
    Log "Link error: $($_.Exception.Message)"
    Start-Sleep -Seconds 2
  } finally {
    try { Send-Frame $script:T.Bye $null | Out-Null } catch { }
    Stop-Blob -Delete $true
    Close-Link
  }
}
