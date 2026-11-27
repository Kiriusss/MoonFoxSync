using System;
using System.Diagnostics;
using System.Collections.Generic;
using System.Drawing;
using System.IO;
using System.Linq;
using System.Runtime.InteropServices;
using System.Security.Principal;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using System.Windows.Forms;

namespace XinhuSync
{
internal sealed partial class WindowsShell : ApplicationContext
{
    List<Record> projects = new List<Record>();
    public List<Record> Projects
    {
        get {
            return projects;
        }
    }
    public string ProjectError { get; private set; }
    Task localRead, upstreamRead;
    long generation, published;
    bool haveProjectList;
    public Task RefreshProjects(bool fetch)
    {
        if (quitting)
            return Task.FromResult(0);
        if (fetch)
        {
            if (upstreamRead == null || upstreamRead.IsCompleted)
                upstreamRead = ReadProjects(true);
            return upstreamRead;
        }
        if (localRead == null || localRead.IsCompleted)
            localRead = ReadProjects(false);
        return localRead;
    }
    public async void RequestProjects()
    {
        await RefreshProjects(false);
    }
    public void ApplyGitState(string id, Record git)
    {
        var p = Projects.FirstOrDefault(r => r.Field("unitId") == id);
        if (p != null)
        {
            p["git"] = git;
            p["gitCheckedAt"] = DateTimeOffset.UtcNow.ToString("o");
            p.Remove("gitError");
            projects = Projects.ToList();
            published = ++generation;
            Notify();
        }
    }
    async Task ReadProjects(bool upstream)
    {
        if (quitting)
            return;
        long request = ++generation;
        try
        {
            var response = upstream ? await Bridge.Call(null, "project", "status", "--refresh-git")
                                    : await Bridge.Call(null, "project", "status");
            if (quitting)
                return;
            if (request < published)
            {
                // Another action changed the bindings while the fetch was running.
                // The Go core now has fresh Git data. Re-read the current bindings.
                if (upstream && PowerSchedule.Current() == ComputerPower.External && (window.Visible || panel.Visible))
                    await RefreshProjects(false);
                return;
            }
            published = request;
            projects = response.Rows()
                           .OrderByDescending(row => ActivityTime(row.Field("lastActivity")))
                           .ThenBy(row => row.Field("name"))
                           .ToList();
            ProjectError = response.Field("warning");
            haveProjectList = true;
        }
        catch (Exception ex)
        {
            if (request >= published)
                ProjectError = ex.Message;
        }
        if (!quitting)
            Notify();
    }
    static DateTimeOffset ActivityTime(string value)
    {
        DateTimeOffset time;
        return DateTimeOffset.TryParse(value, out time) ? time : DateTimeOffset.MinValue;
    }
    public void OpenProject(string id)
    {
        ShowWindow(null);
        window.SelectProject(id);
    }
    readonly MainWindow window;
    readonly NotifyIcon tray;
    readonly TrayPanel panel;
    readonly ContextMenuStrip menu;
    readonly System.Windows.Forms.Timer timer = new System.Windows.Forms.Timer { Interval = 60000 };
    readonly ToolStripMenuItem scheduleItem, checkItem, queueItem, statusItem;
    RegisteredWaitHandle activation;
    SchedulePolicy policy = new SchedulePolicy();
    Process agent;
    bool checking, automaticCheck, checkWhenIdle, quitting;
    ComputerPower power = PowerSchedule.Current();
    DateTime? lastBatteryCheck;
    Task queueRead;
    int policyRevision;
    string error = "";

    public bool Busy
    {
        get {
            return checking;
        }
    }
    public bool AutoSyncEnabled
    {
        get {
            return policy.Enabled;
        }
    }
    public bool AwaySmall
    {
        get {
            return policy.Small;
        }
    }
    public string HomeUrl
    {
        get {
            return policy.Url;
        }
    }
    public string HomeInterface
    {
        get {
            return policy.Adapter;
        }
    }
    public int BatteryMinutes { get { return policy.BatteryMinutes; } }
    public string AutomaticStatus
    {
        get {
            if (!policy.Enabled) return "已暂停";
            if (power == ComputerPower.External) return "接电 · 已启用";
            if (power == ComputerPower.Unknown) return "电源未知 · 仅手动";
            string source = "电池";
            return source + (policy.BatteryMinutes == 0 ? " · 仅手动" :
                             policy.BatteryMinutes == 60 ? " · 每小时" : " · 每四小时");
        }
    }
    public DateTime? LastChecked { get; private set; }
    public int Queued { get; private set; }
    public string Connection { get; private set; }
    public event EventHandler Changed;

    public bool StartOnLogin
    {
        get {
            return ResidentSettings.LoginRegistered;
        }
        set {
            ResidentSettings.SetLogin(value);
            Notify();
        }
    }
    public bool StartupDisabledBySystem
    {
        get {
            return ResidentSettings.SystemDisabled;
        }
    }

    public string StatusText
    {
        get {
            if (Busy)
                return "正在检查绑定来源与队列";
            if (error.Length != 0)
                return error;
            return "自动检查 · " + AutomaticStatus;
        }
    }

    // One auto-reset event per user and session doubles as instance ownership and activation.
    // A signal sent before the primary window finishes initializing remains pending.
    public static void Run(string[] args)
    {
        bool background = args.Any(a => a == "--background");
        string sid;
        using (var identity = WindowsIdentity.GetCurrent()) sid = identity.User.Value;
        bool primary;
        using (var signal = new EventWaitHandle(false, EventResetMode.AutoReset,
                                                @"Local\Kirius.XinhuSync." + sid, out primary))
        {
            if (!primary)
            {
                if (!background)
                {
                    AllowExistingWindow();
                    signal.Set();
                }
                return;
            }
            Application.EnableVisualStyles();
            Application.SetCompatibleTextRenderingDefault(false);
            using (var app = new WindowsShell(signal, background)) Application.Run(app);
        }
    }

    [DllImport("user32.dll")]
    static extern bool AllowSetForegroundWindow(int processId);

    static void AllowExistingWindow()
    {
        using (var current = Process.GetCurrentProcess())
        {
            foreach (var peer in Process.GetProcessesByName(current.ProcessName))
            {
                using (peer)
                {
                    try
                    {
                        if (peer.Id != current.Id && peer.SessionId == current.SessionId &&
                            String.Equals(peer.MainModule.FileName, Application.ExecutablePath,
                                          StringComparison.OrdinalIgnoreCase))
                            AllowSetForegroundWindow(peer.Id);
                    }
                    catch (System.ComponentModel.Win32Exception)
                    {
                    }
                    catch (InvalidOperationException)
                    {
                    }
                }
            }
        }
    }

    WindowsShell(EventWaitHandle signal, bool background)
    {
        Queued = -1;
        Connection = "尚未读取资料库";
        try
        {
            policy = ResidentSettings.ReadPolicy();
            lastBatteryCheck = ResidentSettings.ReadBatteryTime();
            ResidentSettings.InitializeLogin();
        }
        catch (Exception ex)
        {
            error = ex.Message;
        }
        window = new MainWindow(this);
        window.FormClosing += delegate(object sender, FormClosingEventArgs e)
        {
            if (!quitting && e.CloseReason == CloseReason.UserClosing)
            {
                e.Cancel = true;
                window.Hide();
            }
        };
        window.FormClosed += delegate
        {
            ExitThread();
        };

        // A message handle allows login startup with a tray icon and no first-frame window flash.
        var windowHandle = window.Handle;
        menu = new ContextMenuStrip();
        statusItem = (ToolStripMenuItem)menu.Items.Add("状态");
        statusItem.Enabled = false;
        menu.Items.Add("打开主窗口", null, delegate { ShowWindow("overview"); });
        queueItem = (ToolStripMenuItem)menu.Items.Add("打开队列", null, delegate { ShowWindow("queue"); });
        checkItem = (ToolStripMenuItem)menu.Items.Add("立即检查", null, async delegate { await CheckNow(); });
        scheduleItem = (ToolStripMenuItem)menu.Items.Add("自动检查", null, delegate { ToggleAuto(); });
        menu.Items.Add("设置", null, delegate { ShowWindow("settings"); });
        menu.Items.Add(new ToolStripSeparator());
        menu.Items.Add("退出心狐同步", null, delegate { Quit(); });
        tray = new NotifyIcon { Icon = window.Icon ?? SystemIcons.Application, Text = "心狐同步",
                                ContextMenuStrip = menu, Visible = true };
        panel = new TrayPanel(this);
        StartTransfers();
        tray.MouseClick += delegate(object sender, MouseEventArgs e)
        {
            if (e.Button == MouseButtons.Left)
            {
                panel.ShowAt(Cursor.Position);
                RequestProjects();
                RefreshQueue();
            }
        };
        menu.Opening += delegate
        {
            RefreshQueue();
        };
        timer.Tick += async delegate {
            var scan = AutomaticTick(false);
            await RefreshVisibleProjects();
            await scan;
        };
        activation = ThreadPool.RegisterWaitForSingleObject(signal, delegate {
            try
            {
                window.BeginInvoke((Action)delegate {
                    if (!quitting)
                        ShowWindow(null);
                });
            }
            catch (InvalidOperationException)
            {
            }
        }, null, Timeout.Infinite, false);
        window.BeginInvoke((Action)async delegate {
            if (!background)
                ShowWindow(null);
            Notify();
            timer.Start();
            RefreshQueue();
            var scan = AutomaticTick(false);
            await RefreshVisibleProjects();
            await scan;
        });
    }

    void RecordBatteryTime(DateTime? now)
    {
        lastBatteryCheck = now;
        try { ResidentSettings.WriteBatteryTime(now); }
        catch (System.UnauthorizedAccessException ex) { error = ex.Message; }
        catch (System.Security.SecurityException ex) { error = ex.Message; }
        catch (IOException ex) { error = ex.Message; }
    }

    public async void PowerChanged()
    {
        if (!quitting && timer.Enabled) await AutomaticTick(true);
    }

    async Task AutomaticTick(bool powerEvent)
    {
        if (quitting) return;
        ComputerPower previous = power;
        power = PowerSchedule.Current();
        bool changed = power != previous;
        DateTime now = DateTime.UtcNow;
        DateTime? planned = PowerSchedule.BatteryStart(power, policy.Enabled ? policy.BatteryMinutes : 0,
                                                       changed, lastBatteryCheck, now);
        if (planned != lastBatteryCheck) RecordBatteryTime(planned);
        if (changed && power == ComputerPower.External && checking && policy.Enabled) checkWhenIdle = true;
        if (changed) {
            if (power != ComputerPower.External && automaticCheck) {
                policyRevision++;
                StopAgent();
            }
            Notify();
        }
        if ((powerEvent && !changed) || checking || !policy.Enabled ||
            !PowerSchedule.Due(power, policy.BatteryMinutes, lastBatteryCheck, now)) return;
        if (power != ComputerPower.External) RecordBatteryTime(now);
        await Check(true);
    }

    async Task RefreshVisibleProjects()
    {
        if (power != ComputerPower.External || (!window.Visible && !panel.Visible)) return;
        if (!haveProjectList) await RefreshProjects(false);
        if (!quitting && PowerSchedule.Current() == ComputerPower.External && (window.Visible || panel.Visible))
            await RefreshProjects(true);
    }

    public void PublishState(string connection, int queued)
    {
        if (!String.IsNullOrWhiteSpace(connection))
            Connection = connection;
        if (queued >= 0)
            Queued = queued;
        UpdateTray();
    }

    public void ShowWindow(string page)
    {
        if (quitting)
            return;
        panel.Hide();
        if (!window.Visible)
            window.Show();
        if (window.WindowState == FormWindowState.Minimized)
            window.WindowState = FormWindowState.Normal;
        window.Activate();
        window.BringToFront();
        RequestProjects();
        if (page != null)
            window.ShowSection(page);
    }

    public void ToggleAuto()
    {
        try
        {
            Configure(!policy.Enabled, policy.Url, policy.Adapter, policy.Small, policy.BatteryMinutes);
        }
        catch (Exception ex)
        {
            error = ex.Message;
            Notify();
        }
    }

    public void Configure(bool enabled, string url, string adapter, bool small, int batteryMinutes)
    {
        var candidate = SchedulePolicy.Create(enabled, url, adapter, small, batteryMinutes);
        ResidentSettings.WritePolicy(candidate);
        bool changed = policy.Enabled != candidate.Enabled || policy.Small != candidate.Small ||
                       policy.Url != candidate.Url || policy.Adapter != candidate.Adapter ||
                       policy.BatteryMinutes != candidate.BatteryMinutes;
        DateTime? planned = PowerSchedule.BatteryStart(power, candidate.Enabled ? candidate.BatteryMinutes : 0,
            !policy.Enabled || policy.BatteryMinutes != candidate.BatteryMinutes, lastBatteryCheck, DateTime.UtcNow);
        if (planned != lastBatteryCheck) RecordBatteryTime(planned);
        policy = candidate;
        if (!changed)
            return;
        policyRevision++;
        if (automaticCheck) StopAgent();
        error = "";
        Notify();
        if (candidate.Enabled && !Busy)
            window.BeginInvoke((Action)async delegate { await AutomaticTick(false); });
    }

    void UpdateTray()
    {
        if (tray == null || quitting)
            return;
        var active = Transfers == null ? null : Transfers.FirstOrDefault();
        string tip = "心狐同步 · " + (active == null ? StatusText : ProjectTransferProgress.Group(Transfers)[0].Caption);
        tray.Text = tip.Length <= 63 ? tip : tip.Substring(0, 62) + "…";
        statusItem.Text = StatusText.Length <= 56 ? StatusText : StatusText.Substring(0, 55) + "…";
        queueItem.Text = Queued < 0 ? "打开队列" : "打开队列（" + Queued + " 项待上传）";
        checkItem.Enabled = !Busy;
        scheduleItem.Text = policy.Enabled ? "暂停自动检查" : "启用自动检查";
        if (panel != null)
            panel.UpdateValues();
    }

    void Notify()
    {
        if (quitting)
            return;
        UpdateTray();
        if (Changed != null)
            Changed(this, EventArgs.Empty);
    }

    async void RefreshQueue()
    {
        if (quitting || (queueRead != null && !queueRead.IsCompleted))
            return;
        queueRead = ReadQueue();
        await queueRead;
    }

    async Task ReadQueue()
    {
        try
        {
            var response = await Bridge.Call(null, "queue");
            Queued = response.Rows().Count(row => row.Field("state") == "queued");
        }
        catch
        {
            Queued = -1;
        }
        if (!quitting)
            Notify();
    }

    // Consume both pipes concurrently: large projects can emit many JSON receipts.
    static async Task<string> Drain(StreamReader stream, bool keepTail)
    {
        char[] buffer = new char[2048];
        var tail = new StringBuilder();
        int count;
        while ((count = await stream.ReadAsync(buffer, 0, buffer.Length).ConfigureAwait(false)) > 0)
        {
            if (!keepTail)
                continue;
            tail.Append(buffer, 0, count);
            if (tail.Length > 2048)
                tail.Remove(0, tail.Length - 2048);
        }
        return tail.ToString().Trim();
    }

    public Task CheckNow() { return Check(false); }

    async Task Check(bool automatic)
    {
        if (checking || quitting)
            return;
        checking = true;
        automaticCheck = automatic;
        int revision = policyRevision;
        error = "";
        Notify();
        try
        {
            string exe = Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "kdrive-agent.exe");
            var start =
                new ProcessStartInfo(exe, String.Join(" ", policy.AgentArguments().Select(Bridge.Quote))) {
                    UseShellExecute = false,
                    CreateNoWindow = true,
                    RedirectStandardInput = true,
                    RedirectStandardOutput = true,
                    RedirectStandardError = true,
                    StandardOutputEncoding = Encoding.UTF8,
                    StandardErrorEncoding = Encoding.UTF8
                };
            using (var proc = new Process { StartInfo = start })
            {
                proc.Start();
                agent = proc;
                Task<string> stdout = Drain(proc.StandardOutput, false);
                Task<string> stderr = Drain(proc.StandardError, true);
                await Task.WhenAll(stdout, stderr, Task.Run((Action)proc.WaitForExit));
                if (revision == policyRevision && proc.ExitCode != 0)
                    error = String.IsNullOrWhiteSpace(stderr.Result) ? "后台检查失败" : stderr.Result;
            }
        }
        catch (Exception ex)
        {
            if (revision == policyRevision)
                error = ex.Message;
        }
        finally
        {
            agent = null;
            automaticCheck = false;
            checking = false;
            if (!quitting)
            {
                if (revision == policyRevision)
                    LastChecked = DateTime.Now;
                Notify();
                RefreshQueue();
                bool retry = checkWhenIdle || revision != policyRevision;
                checkWhenIdle = false;
                if (retry && policy.Enabled)
                    window.BeginInvoke((Action)async delegate { await AutomaticTick(false); });
            }
        }
    }

    void StopAgent()
    {
        if (agent == null)
            return;
        try
        {
            if (!agent.HasExited)
                agent.StandardInput.Close();
        }
        catch (InvalidOperationException)
        {
        }
        catch (IOException) { /* The agent has already closed its input. */ }
    }

    public void Quit()
    {
        if (quitting)
            return;
        if (window.OperationInProgress || SyncingPending)
        {
            ShowWindow(null);
            MessageBox.Show(window, "手动文件操作仍在进行，完成后可从托盘退出。", "心狐同步");
            return;
        }
        quitting = true;
        timer.Stop();
        StopAgent();
        tray.Visible = false;
        window.Close();
    }

    protected override void Dispose(bool disposing)
    {
        if (disposing)
        {
            quitting = true;
            if (activation != null)
                activation.Unregister(null);
            timer.Dispose();
            StopAgent();
            tray.Visible = false;
            panel.Dispose();
            menu.Dispose();
            tray.Dispose();
            DisposeTransfers();
            window.Dispose();
        }
        base.Dispose(disposing);
    }
}
}
