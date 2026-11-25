# Runs on a fresh Windows runner. It installs and removes both product versions.
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Candidate,
    [Parameter(Mandatory)][string]$Previous,
    [Parameter(Mandatory)][string]$Version,
    [Parameter(Mandatory)][string]$PreviousVersion,
    [Parameter(Mandatory)][string]$LogDir
)
$ErrorActionPreference = 'Stop'

$installer = New-Object -ComObject WindowsInstaller.Installer
$installDir = Join-Path $env:LOCALAPPDATA 'Programs\Moon Fox Sync'
$exe = Join-Path $installDir '心狐同步.exe'
$payload = '心狐同步.exe', '心狐同步.exe.config', 'kdrive.exe', 'kdrive-agent.exe'
$links = [Environment]::GetFolderPath('Programs'), [Environment]::GetFolderPath('DesktopDirectory') |
    ForEach-Object { Join-Path $_ '心狐同步.lnk' }
$wsh = New-Object -ComObject WScript.Shell

function Get-ProductCode($package, $expectedVersion) {
    $database = $installer.OpenDatabase((Convert-Path -LiteralPath $package), 0)
    try {
        $view = $database.OpenView('SELECT `Property`, `Value` FROM `Property`')
        [void]$view.Execute()
        $properties = @{}
        while ($row = $view.Fetch()) { $properties[$row.StringData(1)] = $row.StringData(2) }
        if ($properties.ProductVersion -ne $expectedVersion -or $properties.ProductName -ne 'Moon Fox Sync' -or
            $properties.UpgradeCode -ne '{32E34D01-9EA5-4D9A-AD7C-40D0CA41EB9D}') {
            throw "Unexpected MSI identity for $package ($($properties.ProductName) $($properties.ProductVersion))"
        }
        $properties.ProductCode
    } finally {
        if ($view) { [void]$view.Close(); [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($view) }
        [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($database)
    }
}

function Assert-InstallerUi($package) {
    $database = $installer.OpenDatabase((Convert-Path -LiteralPath $package), 0)
    function Get-UiRows([string]$table, [string[]]$columns) {
        $fields = ($columns | ForEach-Object { '`' + $_ + '`' }) -join ', '
        $view = $database.OpenView(("SELECT $fields FROM " + '`' + $table + '`'))
        try {
            [void]$view.Execute()
            while ($record = $view.Fetch()) {
                $values = for ($i = 1; $i -le $columns.Count; $i++) { $record.StringData($i) }
                $values -join '|'
            }
        } finally {
            [void]$view.Close()
            [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($view)
        }
    }
    function Assert-Rows($table, $columns, $expected) {
        $actual = @(Get-UiRows $table $columns)
        foreach ($row in $expected) {
            if ($row -cnotin $actual) { throw "Installer UI table $table lacks: $row" }
        }
    }
    try {
        Assert-Rows Dialog 'Dialog' @('WelcomeDlg', 'VerifyReadyDlg', 'MaintenanceWelcomeDlg', 'MaintenanceTypeDlg', 'ProgressDlg', 'ExitDialog')
        $sequence = @{}
        foreach ($row in Get-UiRows InstallUISequence @('Action', 'Condition', 'Sequence')) {
            $action, $condition, $order = $row -split '\|', 3
            $sequence[$action] = @{ Condition = $condition; Order = [int]$order }
        }
        if ($sequence.WelcomeDlg.Condition -ne 'NOT Installed OR PATCH' -or
            $sequence.MaintenanceWelcomeDlg.Condition -ne 'Installed AND NOT RESUME AND NOT Preselected AND NOT PATCH' -or
            $sequence.ExitDialog.Order -ne -1 -or $sequence.ProgressDlg.Order -le 0 -or
            $sequence.ProgressDlg.Order -ge $sequence.ExecuteAction.Order) {
            throw 'The interactive setup must route fresh installs, maintenance, progress and successful completion'
        }
        foreach ($action in 'WelcomeDlg', 'MaintenanceWelcomeDlg') {
            if ($sequence[$action].Order -le 0 -or $sequence[$action].Order -ge $sequence.ProgressDlg.Order) {
                throw "$action must appear before installation progress"
            }
        }
        Assert-Rows ControlEvent @('Dialog_', 'Control_', 'Event', 'Argument', 'Condition', 'Ordering') @(
            'WelcomeDlg|Next|NewDialog|VerifyReadyDlg|1|1'
            'MaintenanceWelcomeDlg|Next|NewDialog|MaintenanceTypeDlg|1|1'
            'MaintenanceTypeDlg|RepairButton|NewDialog|VerifyReadyDlg|1|1'
            'MaintenanceTypeDlg|RemoveButton|NewDialog|VerifyReadyDlg|1|1'
            'ExitDialog|Finish|DoAction|LaunchClient|WIXUI_EXITDIALOGOPTIONALCHECKBOX = "1" AND ACTION = "INSTALL" AND NOT Installed AND NOT REMOVE|1'
            'ExitDialog|Finish|EndDialog|Return|1|2'
        )
        Assert-Rows Control @('Dialog_', 'Control', 'Type', 'Property') @('ExitDialog|OptionalCheckBox|CheckBox|WIXUI_EXITDIALOGOPTIONALCHECKBOX')
        Assert-Rows ControlCondition @('Dialog_', 'Control_', 'Action', 'Condition') @('ExitDialog|OptionalCheckBox|Show|WIXUI_EXITDIALOGOPTIONALCHECKBOXTEXT AND NOT Installed')
        Assert-Rows CheckBox @('Property', 'Value') @('WIXUI_EXITDIALOGOPTIONALCHECKBOX|1')
        Assert-Rows Property @('Property', 'Value') @(
            'WIXUI_EXITDIALOGOPTIONALCHECKBOX|1'
            'WIXUI_EXITDIALOGOPTIONALCHECKBOXTEXT|启动心狐同步'
            'WixShellExecTarget|[#GuiFile]'
        )
        Assert-Rows CustomAction @('Action', 'Type', 'Source', 'Target') @('LaunchClient|65|WixCA|WixShellExec')
        Write-Host 'Verified installation, maintenance, completion and optional launch in the compiled MSI dialog tables.'
    } finally {
        [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($database)
    }
}

function Invoke-Msi($operation, $target, $stage) {
    $log = Join-Path $LogDir "$stage.log"
    $proc = Start-Process msiexec.exe -ArgumentList "$operation `"$target`" /qn /norestart /l*v `"$log`"" -PassThru -Wait -WindowStyle Hidden
    if ($proc.ExitCode -ne 0) {
        if (Test-Path -LiteralPath $log) {
            Select-String -Path $log -Pattern 'Return value 3' -Context 15,4 | Out-Host
            Get-Content -LiteralPath $log -Tail 15 | Out-Host
        }
        throw "Windows Installer failed at $stage (exit $($proc.ExitCode))"
    }
    Write-Host "Passed $stage"
}

function Assert-Installed($code, $expectedVersion) {
    if ($installer.ProductState($code) -ne 5 -or $installer.ProductInfo($code, 'VersionString') -ne $expectedVersion) {
        throw "Windows Installer did not register $code as $expectedVersion"
    }
    foreach ($file in $payload) {
        if (!(Test-Path -LiteralPath (Join-Path $installDir $file) -PathType Leaf)) { throw "Missing $file" }
    }
    $appVersion = (Get-Item -LiteralPath $exe).VersionInfo.FileVersionRaw.ToString(3)
    if ($appVersion -ne $expectedVersion) { throw "GUI version $appVersion differs from $expectedVersion" }
    foreach ($link in $links) {
        if (!(Test-Path -LiteralPath $link -PathType Leaf)) { throw "Missing $link" }
        $shortcut = $wsh.CreateShortcut($link)
        if ($shortcut.TargetPath -ne $exe -or $shortcut.WorkingDirectory.TrimEnd('\') -ne $installDir) {
            throw "Invalid shortcut $link : $($shortcut.TargetPath) in $($shortcut.WorkingDirectory)"
        }
    }
}

function Assert-Uninstalled($code) {
    if ($installer.ProductState($code) -ne -1) { throw "Uninstall retained MSI product $code" }
    foreach ($path in @($links) + @($payload | ForEach-Object { Join-Path $installDir $_ })) {
        if (Test-Path -LiteralPath $path) { throw "Uninstall retained $path" }
    }
    $login = Get-ItemPropertyValue 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run' -Name XinhuSync -ErrorAction SilentlyContinue
    if ($login -like "*$exe*") { throw 'Uninstall retained the login entry for the removed executable' }
}

function Test-Gui {
    $started = Get-Date
    $proc = Start-Process -FilePath $exe -WorkingDirectory $installDir -PassThru
    try {
        $timeout = $started.AddSeconds(55)
        while ((Get-Date) -lt $timeout) {
            $proc.Refresh()
            if ($proc.HasExited) { throw "GUI exited with $($proc.ExitCode) before displaying its window" }
            if ($proc.MainWindowHandle -ne [IntPtr]::Zero -and $proc.MainWindowTitle -like '*心狐同步*Moon Fox Sync*' -and $proc.Responding) {
                Write-Host "Opened the installed GUI: $($proc.MainWindowTitle)"
                return
            }
            Start-Sleep -Milliseconds 300
        }
        throw "No responsive Moon Fox Sync main window. Last window: $($proc.MainWindowTitle)"
    } catch {
        Get-WinEvent -FilterHashtable @{ LogName = 'Application'; StartTime = $started; Id = 1000,1026 } -ErrorAction SilentlyContinue |
            Select-Object -First 3 TimeCreated,Message | Format-List | Out-Host
        throw
    } finally {
        if (!$proc.HasExited) { $proc.Kill($true); $proc.WaitForExit() }
        $proc.Dispose()
    }
}

$newCode = Get-ProductCode $Candidate $Version
$oldCode = Get-ProductCode $Previous $PreviousVersion
Assert-InstallerUi $Candidate
if ([version]$Version -le [version]$PreviousVersion -or $oldCode -eq $newCode) { throw 'Expected a major MSI upgrade between distinct products' }
if ($installer.ProductState($newCode) -ne -1 -or $installer.ProductState($oldCode) -ne -1 -or
    (Test-Path -LiteralPath $installDir) -or ($links | Where-Object { Test-Path -LiteralPath $_ })) {
    throw 'Verification requires a clean Windows profile with no existing Moon Fox Sync installation'
}

[void](New-Item -ItemType Directory -Force -Path $LogDir)
Invoke-Msi '/i' $Candidate '01-clean-install'
Assert-Installed $newCode $Version
Test-Gui

# Force repair to restore a missing payload file.
Remove-Item -LiteralPath (Join-Path $installDir '心狐同步.exe.config')
Invoke-Msi '/fa' $Candidate '02-repair'
Assert-Installed $newCode $Version
Invoke-Msi '/x' $newCode '03-clean-uninstall'
Assert-Uninstalled $newCode

Invoke-Msi '/i' $Previous '04-install-previous'
Assert-Installed $oldCode $PreviousVersion
Invoke-Msi '/i' $Candidate '05-upgrade'
Assert-Installed $newCode $Version
if ($installer.ProductState($oldCode) -ne -1) { throw "Upgrade retained the previous MSI registration $oldCode" }
Test-Gui
Invoke-Msi '/x' $newCode '06-upgrade-uninstall'
Assert-Uninstalled $newCode
Write-Host "Verified clean install, repair, launch, upgrade from $PreviousVersion to $Version, and uninstall."
