using System;
using System.Runtime.InteropServices;

namespace XinhuSync
{
internal enum ComputerPower { Unknown, External, Battery }

internal static class PowerSchedule
{
    [StructLayout(LayoutKind.Sequential)]
    struct SystemPowerStatus
    {
        public byte ACLineStatus, BatteryFlag, BatteryLifePercent, SystemStatusFlag;
        public uint BatteryLifeTime, BatteryFullLifeTime;
    }

    [DllImport("kernel32.dll")]
    static extern bool GetSystemPowerStatus(out SystemPowerStatus status);

    public static ComputerPower Current()
    {
        SystemPowerStatus status;
        return GetSystemPowerStatus(out status)
            ? Source(status.ACLineStatus, status.BatteryFlag) : ComputerPower.Unknown;
    }

    internal static ComputerPower Source(byte ac, byte battery)
    {
        if (ac == 1 || battery == 128) return ComputerPower.External;
        return ac == 0 ? ComputerPower.Battery : ComputerPower.Unknown;
    }

    internal static DateTime? BatteryStart(ComputerPower power, int minutes, bool reset, DateTime? last, DateTime now)
    {
        if (power != ComputerPower.Battery || minutes == 0) return null;
        return reset || !last.HasValue || last.Value > now ? now : last;
    }

    internal static bool Due(ComputerPower power, int minutes, DateTime? since, DateTime now)
    {
        if (power == ComputerPower.External) return true;
        if (power != ComputerPower.Battery || minutes == 0 || !since.HasValue) return false;
        return now - since.Value >= TimeSpan.FromMinutes(minutes);
    }
}

}
