param(
    [string]$OutDir = (Join-Path $PSScriptRoot '..\..\dist\windows-amd64'),
    [ValidateSet('amd64', 'arm64')][string]$Arch = 'amd64',
    [switch]$Msi
)
$ErrorActionPreference = 'Stop'
if ($Msi -and $Arch -ne 'amd64') { throw 'MSI 目前用于 Windows x64。' }
$repo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
$out = [IO.Path]::GetFullPath($OutDir)
New-Item -ItemType Directory -Force $out | Out-Null

$dotnet = Get-Command dotnet -ErrorAction SilentlyContinue
if ($dotnet -and (& dotnet --list-sdks)) {
    & dotnet build (Join-Path $PSScriptRoot 'XinhuSync.csproj') -c Release -o $out
    if ($LASTEXITCODE -ne 0) { throw 'Windows 界面编译失败。' }
} else {
    $csc = Join-Path $env:WINDIR 'Microsoft.NET\Framework64\v4.0.30319\csc.exe'
    if (-not (Test-Path $csc)) {
        $csc = Join-Path $env:WINDIR 'Microsoft.NET\Framework\v4.0.30319\csc.exe'
    }
    if (-not (Test-Path $csc)) { throw '需要 .NET SDK 或 .NET Framework C# 编译器。' }
    $productInfo = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid().ToString() + '.cs')
    Push-Location $PSScriptRoot
    try {
        [xml]$project = Get-Content -Encoding UTF8 -Raw -LiteralPath 'XinhuSync.csproj'
        $version = $project.Project.PropertyGroup.Version
        $product = $project.Project.PropertyGroup.Product
        [IO.File]::WriteAllText($productInfo, @"
[assembly: System.Reflection.AssemblyVersion("$version")]
[assembly: System.Reflection.AssemblyFileVersion("$version")]
[assembly: System.Reflection.AssemblyProduct("$product")]
"@)
        $sources = @(Get-ChildItem -LiteralPath $PSScriptRoot -Filter *.cs -File | ForEach-Object { $_.FullName })
        $trayResources = @(Get-ChildItem (Join-Path $repo 'assets/brand/tray/TrayFox*.ico') | ForEach-Object { "/resource:$($_.FullName),$($_.Name)" })
        $wpfDir = Join-Path (Split-Path $csc) 'WPF'
        $xamlResources = @(Get-ChildItem -LiteralPath $PSScriptRoot -Filter *.xaml -File | ForEach-Object { "/resource:$($_.FullName),$($_.Name)" })
        & $csc "/lib:$wpfDir" /r:WindowsBase.dll /r:PresentationCore.dll /r:PresentationFramework.dll /r:WindowsFormsIntegration.dll /r:System.Xaml.dll @xamlResources /nologo /codepage:65001 /utf8output /target:winexe /platform:anycpu /langversion:5 /optimize+ /win32icon:xinhu.ico /win32manifest:application.manifest /resource:xinhu64.png,xinhu64.png /r:System.Drawing.dll /r:System.Windows.Forms.dll /r:System.Web.Extensions.dll "/out:$out\心狐同步.exe" @trayResources @sources $productInfo
        if ($LASTEXITCODE -ne 0) { throw 'Windows 界面编译失败。' }
    } finally {
        Pop-Location
        Remove-Item -LiteralPath $productInfo -ErrorAction SilentlyContinue
    }
    $config = '<configuration><startup><supportedRuntime version="v4.0" sku=".NETFramework,Version=v4.8"/></startup></configuration>'
    [IO.File]::WriteAllText((Join-Path $out '心狐同步.exe.config'), $config)
}

$oldOS=$env:GOOS
$oldArch=$env:GOARCH
$oldCgo=$env:CGO_ENABLED
Push-Location $repo
try {
    $env:GOOS='windows'
    $env:GOARCH=$Arch
    $env:CGO_ENABLED='0'
    foreach ($name in 'kdrive', 'kdrive-agent') {
        & go build -trimpath -o (Join-Path $out "$name.exe") "./cmd/$name"
        if ($LASTEXITCODE -ne 0) { throw "$name 编译失败。" }
    }
} finally {
    $env:GOOS=$oldOS
    $env:GOARCH=$oldArch
    $env:CGO_ENABLED=$oldCgo
    Pop-Location
}
Write-Output $out


if ($Msi) {
    $packager = Join-Path $repo 'scripts/package-windows-msi.py'
    $installer = Join-Path (Split-Path -Parent $out) 'MoonFoxSync-Windows-x64.msi'
    if (Get-Command py -ErrorAction SilentlyContinue) {
        & py -3 $packager --input $out --output $installer
    } else {
        & python $packager --input $out --output $installer
    }
    if ($LASTEXITCODE -ne 0) { throw 'Windows MSI 打包失败。' }
}
