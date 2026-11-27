using System;
using System.Collections.Generic;
using System.IO;
using System.Net;
using System.Windows.Forms;
using System.Web.Script.Serialization;
using Microsoft.Win32;

namespace XinhuSync
{
    internal sealed class SchedulePolicy
    {
        public bool Enabled { get; set; }
        public string Url { get; set; }
        public string Adapter { get; set; }
        public bool Small { get; set; }
        public int BatteryMinutes { get; set; }

        public SchedulePolicy() { Url = ""; Adapter = ""; }

        public static SchedulePolicy Create(bool enabled, string url, string adapter, bool small, int batteryMinutes)
        {
            if (batteryMinutes != 0 && batteryMinutes != 60 && batteryMinutes != 240)
                throw new ArgumentException("电池模式请选仅手动、每小时或每四小时。");
            url = (url ?? "").Trim();
            adapter = (adapter ?? "").Trim();
            if ((url.Length == 0) != (adapter.Length == 0))
                throw new ArgumentException("请同时填写家庭 NAS 地址和物理网卡名称。");
            if (url.Length != 0)
            {
                Uri uri;
                IPAddress address;
                if (!Uri.TryCreate(url, UriKind.Absolute, out uri) || uri.Scheme != "https" ||
                    !String.IsNullOrEmpty(uri.UserInfo) || uri.AbsolutePath != "/" ||
                    !String.IsNullOrEmpty(uri.Query) || !String.IsNullOrEmpty(uri.Fragment) ||
                    !IPAddress.TryParse(uri.Host, out address) || !PrivateV4(address))
                    throw new ArgumentException("家庭 NAS 需要使用带端口或默认端口的 HTTPS 私网 IPv4 地址。");
                url = uri.GetLeftPart(UriPartial.Authority);
            }
            return new SchedulePolicy { Enabled = enabled, Url = url, Adapter = adapter, Small = small, BatteryMinutes = batteryMinutes };
        }

        static bool PrivateV4(IPAddress ip)
        {
            byte[] b = ip.GetAddressBytes();
            return b.Length == 4 && (b[0] == 10 || (b[0] == 172 && b[1] >= 16 && b[1] <= 31) ||
                                     (b[0] == 192 && b[1] == 168));
        }

        public void AppendHomeRoute(List<string> args)
        {
            if (!String.IsNullOrEmpty(Url))
                args.AddRange(new[] { "--home-url", Url, "--home-interface", Adapter });
        }

        public string[] AgentArguments()
        {
            var args = new List<string> { "--once", "--cancel-on-stdin-close" };
            AppendHomeRoute(args);
            if (Small) args.Add("--away-small");
            return args.ToArray();
        }
    }

    internal static class ResidentSettings
    {
        const string AppKey = @"Software\Kirius\XinhuSync\Desktop";
        const string RunKey = @"Software\Microsoft\Windows\CurrentVersion\Run";
        const string ApprovalKey = @"Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run";
        const string ValueName = "XinhuSync";

        public static SchedulePolicy ReadPolicy()
        {
            using (var key = Registry.CurrentUser.OpenSubKey(AppKey))
            {
                string raw = key == null ? null : key.GetValue("Schedule") as string;
                if (String.IsNullOrEmpty(raw)) return new SchedulePolicy();
                var policy = new JavaScriptSerializer().Deserialize<SchedulePolicy>(raw);
                return SchedulePolicy.Create(policy.Enabled, policy.Url, policy.Adapter, policy.Small, policy.BatteryMinutes);
            }
        }

        public static void WritePolicy(SchedulePolicy policy)
        {
            using (var key = Registry.CurrentUser.CreateSubKey(AppKey))
                key.SetValue("Schedule", new JavaScriptSerializer().Serialize(policy));
        }

        public static DateTime? ReadBatteryTime()
        {
            using (var key = Registry.CurrentUser.OpenSubKey(AppKey))
            {
                object stored = key == null ? null : key.GetValue("LastBatteryCheckUtc");
                if (!(stored is long)) return null;
                long ticks = (long)stored;
                if (ticks <= 0 || ticks > DateTime.MaxValue.Ticks) return null;
                return new DateTime(ticks, DateTimeKind.Utc);
            }
        }

        public static void WriteBatteryTime(DateTime? time)
        {
            using (var key = Registry.CurrentUser.CreateSubKey(AppKey))
                if (time.HasValue) key.SetValue("LastBatteryCheckUtc", time.Value.Ticks, RegistryValueKind.QWord);
                else key.DeleteValue("LastBatteryCheckUtc", false);
        }

        static string Command { get { return Bridge.Quote(Application.ExecutablePath) + " --background"; } }

        static bool IsTemporaryPath()
        {
            string temporary = Path.GetFullPath(Path.GetTempPath()).TrimEnd(Path.DirectorySeparatorChar) + Path.DirectorySeparatorChar;
            return Path.GetFullPath(Application.ExecutablePath).StartsWith(temporary, StringComparison.OrdinalIgnoreCase);
        }

        static string RunEntry()
        {
            using (var key = Registry.CurrentUser.OpenSubKey(RunKey))
                return key == null ? null : key.GetValue(ValueName) as string;
        }

        public static bool LoginRegistered
        {
            get { return String.Equals(Command, RunEntry(), StringComparison.OrdinalIgnoreCase); }
        }

        // Approval belongs to the Windows startup-apps UI. Only inspect it, never rewrite it.
        public static bool SystemDisabled
        {
            get
            {
                using (var key = Registry.CurrentUser.OpenSubKey(ApprovalKey))
                {
                    var state = key == null ? null : key.GetValue(ValueName) as byte[];
                    return state != null && state.Length >= 4 && (state[0] == 3 || state[0] == 7);
                }
            }
        }

        public static void InitializeLogin()
        {
            if (IsTemporaryPath() || SystemDisabled) return;
            using (var key = Registry.CurrentUser.CreateSubKey(AppKey))
            {
                string previous = key.GetValue("LastLoginCommand") as string;
                string current = RunEntry();
                if (previous == null && current == null)
                    SetLogin(true);
                else if (!String.IsNullOrEmpty(previous) && String.Equals(previous, current, StringComparison.OrdinalIgnoreCase)
                         && !String.Equals(current, Command, StringComparison.OrdinalIgnoreCase))
                    SetLogin(true); // User moved the portable installation and reopened it at its new location.
            }
        }

        public static void SetLogin(bool enabled)
        {
            if (enabled && (IsTemporaryPath() || Command.Length > 260))
                throw new InvalidOperationException("请把桌面包解压到一个较短且固定的目录，重新打开后再启用登录自启。");
            using (var settings = Registry.CurrentUser.CreateSubKey(AppKey))
            using (var key = Registry.CurrentUser.CreateSubKey(RunKey))
            {
                if (enabled) key.SetValue(ValueName, Command, RegistryValueKind.String);
                else key.DeleteValue(ValueName, false);
                settings.SetValue("LastLoginCommand", enabled ? Command : "");
            }
        }
    }
}
