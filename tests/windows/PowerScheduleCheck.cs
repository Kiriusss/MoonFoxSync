// Compile with client/windows/PowerSchedule.cs to test power and interval decisions.
using System;
using XinhuSync;

class PowerScheduleCheck
{
    static void Check(bool condition)
    {
        if (!condition) throw new Exception("Unexpected sync scheduling decision");
    }

    static void Main()
    {
        DateTime now = new DateTime(2026, 11, 27, 8, 0, 0, DateTimeKind.Utc);
        Check(PowerSchedule.Source(1, 8) == ComputerPower.External);
        Check(PowerSchedule.Source(0, 1) == ComputerPower.Battery);
        Check(PowerSchedule.Source(255, 128) == ComputerPower.External);
        Check(PowerSchedule.Source(255, 255) == ComputerPower.Unknown);
        Check(PowerSchedule.BatteryStart(ComputerPower.Battery, 60, true, now.AddDays(-2), now) == now);
        Check(PowerSchedule.BatteryStart(ComputerPower.Battery, 60, false, null, now) == now);
        Check(PowerSchedule.BatteryStart(ComputerPower.Battery, 60, false, now.AddHours(2), now) == now);
        Check(PowerSchedule.BatteryStart(ComputerPower.Battery, 60, false, now.AddMinutes(-30), now) == now.AddMinutes(-30));
        Check(!PowerSchedule.BatteryStart(ComputerPower.External, 60, true, now, now).HasValue);
        Check(!PowerSchedule.BatteryStart(ComputerPower.Battery, 0, false, now, now).HasValue);
        foreach (int minutes in new[] { 0, 60, 240 })
        {
            Check(PowerSchedule.Due(ComputerPower.External, minutes, now, now));
            Check(!PowerSchedule.Due(ComputerPower.Unknown, minutes, now.AddDays(-1), now));
            Check(!PowerSchedule.Due(ComputerPower.Battery, minutes, null, now));
        }
        Check(!PowerSchedule.Due(ComputerPower.Battery, 0, now.AddDays(-1), now));
        foreach (int minutes in new[] { 60, 240 })
        {
            Check(!PowerSchedule.Due(ComputerPower.Battery, minutes, now, now.AddSeconds(-1)));
            Check(!PowerSchedule.Due(ComputerPower.Battery, minutes, now, now.AddMinutes(minutes).AddTicks(-1)));
            Check(PowerSchedule.Due(ComputerPower.Battery, minutes, now, now.AddMinutes(minutes)));
        }
        Console.WriteLine("Passed battery, AC, missing power status and cooldown checks.");
    }
}
