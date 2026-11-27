using System;
using System.Runtime.InteropServices;
using System.Windows.Forms;

namespace XinhuSync
{
// Windows sends AC/DC-source notifications to this window while it is hidden in the tray.
internal sealed partial class MainWindow
{
    static readonly Guid AcDcSource = new Guid("5D3E9A59-E9D5-4B00-A6BD-FF34FF516548");
    IntPtr powerNotification;
    [DllImport("user32.dll")]
    static extern IntPtr RegisterPowerSettingNotification(IntPtr window, ref Guid setting, uint flags);
    [DllImport("user32.dll")]
    static extern bool UnregisterPowerSettingNotification(IntPtr registration);

    protected override void OnHandleCreated(EventArgs e)
    {
        base.OnHandleCreated(e);
        Guid setting = AcDcSource;
        powerNotification = RegisterPowerSettingNotification(Handle, ref setting, 0);
    }

    protected override void OnHandleDestroyed(EventArgs e)
    {
        if (powerNotification != IntPtr.Zero) {
            UnregisterPowerSettingNotification(powerNotification);
            powerNotification = IntPtr.Zero;
        }
        base.OnHandleDestroyed(e);
    }

    protected override void WndProc(ref Message message)
    {
        const int PowerBroadcast = 0x218, PowerStatusChange = 10, ResumeSuspend = 7,
                  ResumeAutomatic = 18, PowerSettingChange = 0x8013;
        base.WndProc(ref message);
        if (message.Msg != PowerBroadcast || shell == null) return;
        long change = message.WParam.ToInt64();
        if (change == PowerStatusChange || change == PowerSettingChange ||
            change == ResumeSuspend || change == ResumeAutomatic)
            shell.PowerChanged();
    }
}
}
