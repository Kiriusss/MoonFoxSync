using System;
using System.Collections.Generic;
using System.Drawing;
using System.Linq;
using System.Reflection;
using System.Threading.Tasks;
using System.Windows.Forms;

namespace XinhuSync
{
internal sealed partial class MainWindow
{
    readonly Dictionary<string, Control> pages = new Dictionary<string, Control>();
    readonly Dictionary<string, NavItem> navigation = new Dictionary<string, NavItem>();
    readonly Label heading = Style.Text("概览", 23, true, Style.Ink),
                   connectionBadge = Style.Text("", 9, true, Style.Plum),
                   endpoint = Style.Text("", 10, false, Style.Muted),
                   libValue = Style.Text("—", 18, true, Style.Ink),
                   queueValue = Style.Text("—", 18, true, Style.Ink),
                   modeValue = Style.Text("—", 16, true, Style.Ink),
                   libEmpty = Style.Text("", 9, false, Style.Muted),
                   cacheEmpty = Style.Text("", 9, false, Style.Muted),
                   queueEmpty = Style.Text("", 9, false, Style.Muted),
                   scanStatus = Style.Text("", 9, false, Style.Muted),
                   startupMessage = Style.Text("", 9, false, Style.Muted);
    readonly TableLayoutPanel activity = new TableLayoutPanel();
    readonly FlowLayoutPanel writeTools = Bar();
    readonly CheckBox login = Check("登录 Windows 时启动"), automatic = Check("每分钟检查绑定来源"),
                      away = Check("外出时自动上传小任务");
    readonly TextBox homeUrl = new TextBox(), adapter = new TextBox();
    Button serverAction, savePolicy, mergeFork, libraryTimeline, syncPending, syncSelected, syncErrors;
    bool settingFields;
    bool? previousEnabled;
    string lastConnection;
    int lastQueued = Int32.MinValue;

    static ComboBox Picker(int width)
    {
        return new ComboBox { Width = width, DropDownStyle = ComboBoxStyle.DropDownList,
                              FlatStyle = FlatStyle.Flat, Margin = new Padding(0, 5, 12, 2) };
    }
    static ListView Grid(params string[] labels)
    {
        var grid =
            new ListView { Dock = DockStyle.Fill,   View = View.Details,   FullRowSelect = true,
                           MultiSelect = false,     HideSelection = false, BorderStyle = BorderStyle.None,
                           BackColor = Style.Paper, ForeColor = Style.Ink };
        foreach (var name in labels)
            grid.Columns.Add(name);
        grid.Resize += delegate
        {
            if (grid.ClientSize.Width < 10)
                return;
            int width = Math.Max(120, grid.ClientSize.Width - SystemInformation.VerticalScrollBarWidth - 3);
            for (int i = 0; i < labels.Length; i++)
                grid.Columns[i].Width = i == 0 ? width * 42 / 100 : width * 58 / (100 * (labels.Length - 1));
        };
        return grid;
    }
    static CheckBox Check(string text)
    {
        return new CheckBox { Text = text,
                              AutoSize = true,
                              ForeColor = Style.Ink,
                              Anchor = AnchorStyles.Left,
                              Margin = new Padding(0, 4, 0, 4),
                              Cursor = Cursors.Hand };
    }
    static FlowLayoutPanel Bar()
    {
        return new FlowLayoutPanel { Dock = DockStyle.Top,   AutoSize = true,
                                     WrapContents = true,    BackColor = Color.Transparent,
                                     Margin = Padding.Empty, Padding = Padding.Empty };
    }
    static Label Hint(string text)
    {
        var l = Style.Text(text, 9, false, Style.Muted);
        l.AutoSize = true;
        l.Margin = new Padding(0, 8, 7, 4);
        return l;
    }
    static TableLayoutPanel Column()
    {
        var p = new TableLayoutPanel {
            Dock = DockStyle.Fill,  ColumnCount = 1,         RowCount = 0,
            Margin = Padding.Empty, Padding = Padding.Empty, BackColor = Color.Transparent
        };
        p.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        return p;
    }
    static void Row(TableLayoutPanel p, Control c, float height)
    {
        int i = p.RowCount++;
        p.RowStyles.Add(height < 0    ? new RowStyle(SizeType.Percent, 100)
                        : height == 0 ? new RowStyle(SizeType.AutoSize)
                                      : new RowStyle(SizeType.Absolute, height));
        p.Controls.Add(c, 0, i);
    }
    Button Action(string text, Func<Task> action, bool primary = false)
    {
        return Style.Action(text, async delegate { await Run(action); }, primary);
    }
    static Label Occupy(Label l)
    {
        l.Dock = DockStyle.Fill;
        return l;
    }
    static string LocalTime(string value)
    {
        DateTimeOffset dt;
        return DateTimeOffset.TryParse(value, out dt) ? dt.ToLocalTime().ToString("yyyy-MM-dd HH:mm") : value;
    }
    internal static string FormatBytes(string raw)
    {
        long n;
        if (!Int64.TryParse(raw, out n))
            return "";
        if (n < 1024)
            return n + " B";
        if (n < 1024L * 1024)
            return String.Format("{0:0.#} KiB", n / 1024d);
        if (n < 1024L * 1024 * 1024)
            return String.Format("{0:0.#} MiB", n / (1024d * 1024));
        return String.Format("{0:0.##} GiB", n / (1024d * 1024 * 1024));
    }
    string LibraryName(string id)
    {
        var match = libraries.FirstOrDefault(x => x.Field("unitId") == id);
        return match == null ? id : match.Field("name");
    }
    static string StateText(string state)
    {
        switch (state)
        {
        case "queued":
            return "待上传";
        case "published":
            return "已发布";
        case "fork":
            return "已保留分叉";
        default:
            return state;
        }
    }
    static string JobTitle(Record r)
    {
        return r.Field("label") == "" ? "自动快照" : r.Field("label");
    }

    void BuildWindow()
    {
        Text = "心狐同步";
        Icon = Icon.ExtractAssociatedIcon(Application.ExecutablePath);
        Font = Style.Type(9);
        ForeColor = Style.Ink;
        BackColor = Style.Canvas;
        AutoScaleDimensions = new SizeF(96, 96);
        AutoScaleMode = AutoScaleMode.Dpi;
        StartPosition = FormStartPosition.CenterScreen;
        ClientSize = new Size(1070, 730);
        MinimumSize = new Size(890, 600);
        var layout = new TableLayoutPanel { Dock = DockStyle.Fill, Margin = Padding.Empty, ColumnCount = 2,
                                            RowCount = 1 };
        layout.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, 192));
        layout.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        layout.RowStyles.Add(new RowStyle(SizeType.Percent, 100));
        Controls.Add(layout);
        var rail = Column();
        rail.BackColor = Style.Rail;
        rail.Paint += delegate(object sender, PaintEventArgs e)
        {
            using (var pen = new Pen(Style.Line))
                e.Graphics.DrawLine(pen, rail.Width - 1, 0, rail.Width - 1, rail.Height);
        };
        var brand = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 2, RowCount = 1,
                                           Padding = new Padding(15, 10, 6, 8), Margin = Padding.Empty };
        brand.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, 48));
        brand.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        var avatar = new PictureBox { Dock = DockStyle.Fill, SizeMode = PictureBoxSizeMode.Zoom,
                                      Margin = new Padding(0, 0, 8, 0) };
        using (var stream = Assembly.GetExecutingAssembly().GetManifestResourceStream(
                   "xinhu64.png")) if (stream != null) using (var image = Image.FromStream(stream))
            avatar.Image = new Bitmap(image);
        brand.Controls.Add(avatar, 0, 0);
        brand.Controls.Add(Occupy(Style.Text("心狐同步", 13, true, Style.Ink)), 1, 0);
        Row(rail, brand, 84);
        AddNav(rail, "overview", "概览");
        AddNav(rail, "projects", "项目");
        AddNav(rail, "libraries", "资料库");
        AddNav(rail, "queue", "同步队列");
        AddNav(rail, "cache", "下载缓存");
        Row(rail, new Panel(), -1);
        AddNav(rail, "settings", "设置");
        Row(rail, new Panel(), 14);
        layout.Controls.Add(rail, 0, 0);
        var workspace = Column();
        workspace.Padding = new Padding(24, 16, 24, 9);
        var head = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 2, RowCount = 1,
                                          Margin = Padding.Empty };
        head.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        head.ColumnStyles.Add(new ColumnStyle(SizeType.AutoSize));
        head.Controls.Add(Occupy(heading), 0, 0);
        var refresh = Action("刷新", RefreshPage);
        refresh.Anchor = AnchorStyles.Right;
        head.Controls.Add(refresh, 1, 0);
        Row(workspace, head, 63);
        AddTransferPanel(workspace);
        pages.Add("overview", Overview());
        pages.Add("projects", ProjectsPage());
        pages.Add("libraries", LibraryPage());
        pages.Add("queue", QueuePage());
        pages.Add("cache", CachePage());
        pages.Add("settings", SettingsPage());
        foreach (var view in pages.Values)
        {
            view.Visible = false;
            content.Controls.Add(view);
        }
        Row(workspace, content, -1);
        Row(workspace, Occupy(status), 30);
        layout.Controls.Add(workspace, 1, 0);
        DisplayPage("overview");
    }
    void AddNav(TableLayoutPanel rail, string key, string name)
    {
        var button = new NavItem(key, name);
        button.Click += delegate
        {
            ShowSection(key);
        };
        navigation.Add(key, button);
        Row(rail, button, 44);
    }
    void DisplayPage(string target)
    {
        page = target;
        foreach (var p in pages)
            p.Value.Visible = p.Key == target;
        foreach (var b in navigation)
            b.Value.Active = b.Key == target;
        heading.Text = navigation[target].Text;
        status.Text = "";
        pages[target].BringToFront();
    }
    Control Overview()
    {
        var p = Column();
        var hero = new Surface { Margin = new Padding(0, 0, 0, 14), BackColor = Color.FromArgb(253, 248, 249),
                                 Padding = new Padding(16, 12, 16, 12) };
        var h = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 3, RowCount = 1,
                                       Margin = Padding.Empty };
        h.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, 58));
        h.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        h.ColumnStyles.Add(new ColumnStyle(SizeType.AutoSize));
        var icon = Style.Symbol("server", Style.Plum);
        icon.Size = new Size(44, 44);
        icon.BackColor = Style.Blush;
        icon.Anchor = AnchorStyles.Left;
        h.Controls.Add(icon, 0, 0);
        var connectionBox = Column();
        Row(connectionBox, Occupy(connectionBadge), 22);
        Row(connectionBox, Occupy(Style.Text("梦枢天罗", 17, true, Style.Ink)), 35);
        Row(connectionBox, Occupy(endpoint), 23);
        h.Controls.Add(connectionBox, 1, 0);
        serverAction = Action("配对服务器", async delegate {
            if (paired)
                await RefreshConnection();
            else
                await Pair();
        }, true);
        serverAction.Anchor = AnchorStyles.Right;
        h.Controls.Add(serverAction, 2, 0);
        hero.Controls.Add(h);
        Row(p, hero, 126);
        var stats = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 3, RowCount = 1,
                                           Margin = Padding.Empty };
        for (int i = 0; i < 3; i++)
            stats.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100f / 3));
        stats.Controls.Add(Metric("资料库", libValue, "libraries", true), 0, 0);
        stats.Controls.Add(Metric("待上传快照", queueValue, "queue", true), 1, 0);
        stats.Controls.Add(Metric("自动同步", modeValue, "settings", false), 2, 0);
        Row(p, stats, 103);
        var recent = new Surface { Padding = new Padding(15, 8, 15, 8), Margin = new Padding(0, 2, 0, 0) };
        var inside = Column();
        var title = Bar();
        var name = Style.Text("近期项目", 12, true, Style.Ink);
        name.Width = 120;
        name.Height = 36;
        title.Controls.Add(name);
        var go = Style.Action("查看项目", delegate { ShowSection("projects"); });
        title.Controls.Add(go);
        Row(inside, title, 46);
        activity.Dock = DockStyle.Fill;
        activity.ColumnCount = 3;
        activity.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        activity.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, 90));
        activity.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, 136));
        activity.Margin = Padding.Empty;
        activity.AutoScroll = true;
        Row(inside, activity, -1);
        recent.Controls.Add(inside);
        Row(p, recent, -1);
        return p;
    }
    Control Metric(string name, Label number, string dest, bool gap)
    {
        var card = new Surface { Margin = new Padding(0, 0, gap ? 10 : 0, 13),
                                 Padding = new Padding(12, 9, 12, 9), Cursor = Cursors.Hand };
        var stack = Column();
        var first = Bar();
        var symbol = Style.Symbol(dest, Style.Plum);
        symbol.Size = new Size(24, 24);
        symbol.Margin = new Padding(0, 0, 4, 0);
        var caption = Style.Text(name, 9, false, Style.Muted);
        caption.Size = new Size(82, 24);
        first.Controls.Add(symbol);
        first.Controls.Add(caption);
        Row(stack, first, 25);
        Row(stack, Occupy(number), -1);
        card.Controls.Add(stack);
        foreach (Control c in new Control[] { card, stack, first, symbol, caption, number })
            c.Click += delegate
            {
                ShowSection(dest);
            };
        return card;
    }
    void UpdateOverview()
    {
        if (serverAction == null)
            return;
        int queued = queueKnown ? allJobs.Count(x => x.Field("state") == "queued") : -1;
        queueValue.Text = queued < 0 ? "—" : queued.ToString();
        libValue.Text = libraryCountKnown ? libraries.Count.ToString() : "—";
        modeValue.Text = shell.Busy ? "检查中" : shell.AutoSyncEnabled ? "已开启" : "已暂停";
        connectionBadge.Text = connection;
        endpoint.Text = String.IsNullOrEmpty(serverAddress) ? "连接 NAS 后可访问获授权的资料库" : serverAddress;
        serverAction.Text = paired ? "检查连接" : "配对服务器";
        if (lastConnection != connection || queued != lastQueued)
        {
            lastConnection = connection;
            lastQueued = queued;
            shell.PublishState(connection, queued);
        }
        if (syncPending != null)
        {
            syncPending.Enabled = shell.CanSyncPending;
            syncPending.Text = shell.SyncButtonText;
            syncSelected.Enabled = !shell.SyncingPending;
            syncErrors.Visible = !String.IsNullOrEmpty(shell.SyncErrorDetails);
            queueEmpty.Text = !String.IsNullOrEmpty(shell.SyncResult) ? shell.SyncResult
                : queueKnown && allJobs.Count == 0 ? "还没有本机快照"
                : shell.Queued == 0 ? "队列中没有待上传的快照" : "";
        }
        activity.SuspendLayout();
        try
        {
            while (activity.Controls.Count > 0)
                activity.Controls[0].Dispose();
            activity.RowStyles.Clear();
            activity.RowCount = 0;
            foreach (var r in shell.Projects.Take(5))
            {
                int n = activity.RowCount++;
                activity.RowStyles.Add(new RowStyle(SizeType.Absolute, 35));
                var projectLink = Style.Action(r.Field("name"), delegate { SelectProject(r.Field("unitId")); });
                projectLink.TextAlign = ContentAlignment.MiddleLeft;
                projectLink.AutoSize = false;
                projectLink.Dock = DockStyle.Fill;
                projectLink.FlatAppearance.BorderSize = 0;
                activity.Controls.Add(projectLink, 0, n);
                activity.Controls.Add(Occupy(Style.Text(ProjectDisplay.Nas(r), 9, false, Style.Plum)), 1, n);
                activity.Controls.Add(
                    Occupy(Style.Text("Git · " + ProjectDisplay.Git(r), 9, false, Style.Muted)), 2, n);
            }
            if (shell.Projects.Count == 0)
            {
                activity.RowCount = 1;
                activity.RowStyles.Add(new RowStyle(SizeType.Absolute, 40));
                var empty = Occupy(Style.Text(!String.IsNullOrEmpty(shell.ProjectError)
                                                  ? shell.ProjectError
                                                  : "暂无项目，可在「项目」中导入",
                                              10, false, Style.Muted));
                activity.Controls.Add(empty, 0, 0);
                activity.SetColumnSpan(empty, 3);
            }
            activity.RowCount++;
            activity.RowStyles.Add(new RowStyle(SizeType.Percent, 100));
        }
        finally
        {
            activity.ResumeLayout();
        }
    }

    static Control GridCard(ListView grid, Label empty)
    {
        var card = new Surface { Margin = Padding.Empty, Padding = new Padding(10) };
        var body = Column();
        Row(body, grid, -1);
        Row(body, Occupy(empty), 24);
        card.Controls.Add(body);
        return card;
    }
    Control LibraryPage()
    {
        var pageBody = Column();
        var library = Bar();
        library.Controls.Add(Hint("资料库"));
        library.Controls.Add(units);
        library.Controls.Add(Action("配对服务器…", Pair));
        libraryTimeline = Action("版本时间线…", LibraryTimeline);
        libraryTimeline.Visible = false;
        library.Controls.Add(libraryTimeline);
        Row(pageBody, library, 0);
        var versionBar = Bar();
        versionBar.Controls.Add(Hint("版本"));
        versionBar.Controls.Add(versions);
        versionBar.Controls.Add(Action("上一级", Up));
        versionBar.Controls.Add(Action("打开选中项", OpenEntry));
        mergeFork = Action("手动合并分叉…", MergeSelectedVersion);
        mergeFork.Visible = false;
        versionBar.Controls.Add(mergeFork);
        Row(pageBody, versionBar, 0);
        writeTools.Controls.Add(Action("绑定项目…", delegate { return Bind(false); }));
        writeTools.Controls.Add(Action("绑定文件…", delegate { return Bind(true); }));
        writeTools.Controls.Add(Hint("版本名"));
        writeTools.Controls.Add(label);
        writeTools.Controls.Add(Action("保存快照", CaptureVersion, true));
        writeTools.Controls.Add(Action("移除本机记录…", delegate { return RemoveLibrary("forget", UnitId); }));
        writeTools.Controls.Add(Action("从 NAS 删除…", delegate { Writer(); return RemoveLibrary("delete", UnitId); }));
        writeTools.Visible = false;
        Row(pageBody, writeTools, 0);
        Row(pageBody, Occupy(breadcrumb), 32);
        Row(pageBody, GridCard(files, libEmpty), -1);
        return pageBody;
    }
    Control QueuePage()
    {
        var p = Column();
        var bar = Bar();
        syncPending = Style.Action("立即同步", async delegate { await shell.SyncPending(); }, true);
        syncPending.Enabled = shell.CanSyncPending;
        syncSelected = Action("上传选中快照", SyncJob);
        syncErrors = Style.Action("查看失败详情", delegate {
            using (var dialog = new Form { Text = "同步失败详情", StartPosition = FormStartPosition.CenterParent,
                                           ClientSize = new Size(560, 260), Font = Font })
            {
                dialog.Controls.Add(new TextBox { Text = shell.SyncErrorDetails, ReadOnly = true, Multiline = true,
                                                 ScrollBars = ScrollBars.Vertical, Dock = DockStyle.Fill });
                dialog.ShowDialog(this);
            }
        });
        syncErrors.Visible = false;
        bar.Controls.Add(syncPending);
        bar.Controls.Add(syncSelected);
        bar.Controls.Add(syncErrors);
        Row(p, bar, 44);
        Row(p, GridCard(jobs, queueEmpty), -1);
        return p;
    }
    Control CachePage()
    {
        var p = Column();
        var bar = Bar();
        bar.Controls.Add(Action("查看或取回", ReadCache, true));
        bar.Controls.Add(Action("释放选中缓存", Evict));
        Row(p, bar, 44);
        Row(p, GridCard(cache, cacheEmpty), -1);
        return p;
    }

    Control SettingsPage()
    {
        var pageBody = new Panel { Dock = DockStyle.Fill, AutoScroll = true, BackColor = Style.Canvas };
        var settings = Column();
        settings.Dock = DockStyle.Top;
        settings.AutoSize = true;
        settings.AutoSizeMode = AutoSizeMode.GrowAndShrink;
        var sessionCard =
            new Surface { Padding = new Padding(16, 8, 12, 6), Margin = new Padding(0, 0, 0, 12) };
        var session = Column();
        Row(session, Occupy(Style.Text("启动与驻留", 12, true, Style.Ink)), 29);
        Row(session, login, 28);
        Row(session, Occupy(startupMessage), 26);
        sessionCard.Controls.Add(session);
        Row(settings, sessionCard, 115);
        login.CheckedChanged += delegate
        {
            if (settingFields)
                return;
            try
            {
                shell.StartOnLogin = login.Checked;
            }
            catch (Exception ex)
            {
                MessageBox.Show(this, ex.Message, "登录启动");
            }
            ReadSettings(false);
        };
        var policyCard = new Surface { Padding = new Padding(16, 10, 16, 10), Margin = Padding.Empty };
        var policy = Column();
        Row(policy, Occupy(Style.Text("同步策略", 12, true, Style.Ink)), 32);
        Row(policy, automatic, 30);
        var fieldBox = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 2, RowCount = 2,
                                              Margin = Padding.Empty };
        fieldBox.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, 116));
        fieldBox.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        fieldBox.RowStyles.Add(new RowStyle(SizeType.Percent, 50));
        fieldBox.RowStyles.Add(new RowStyle(SizeType.Percent, 50));
        fieldBox.Controls.Add(Occupy(Style.Text("家庭 HTTPS 地址", 9, false, Style.Muted)), 0, 0);
        fieldBox.Controls.Add(Occupy(Style.Text("物理网卡名称", 9, false, Style.Muted)), 0, 1);
        homeUrl.Anchor = adapter.Anchor = AnchorStyles.Left | AnchorStyles.Right;
        fieldBox.Controls.Add(homeUrl, 1, 0);
        fieldBox.Controls.Add(adapter, 1, 1);
        Row(policy, fieldBox, 75);
        Row(policy, away, 28);
        Row(policy,
            Occupy(Style.Text("每个任务最多 10 MiB，滚动 24 小时最多 100 MiB。", 9, false, Style.Muted)), 24);
        var buttons = Bar();
        savePolicy = Action("保存策略", delegate {
            shell.Configure(automatic.Checked, homeUrl.Text.Trim(), adapter.Text.Trim(), away.Checked);
            ReadSettings(true);
            status.Text = "同步策略已保存";
            return Task.FromResult(0);
        }, true);
        buttons.Controls.Add(savePolicy);
        buttons.Controls.Add(Action("立即检查", shell.CheckNow));
        buttons.Controls.Add(Action("按来源设置…", SourceSchedules));
        Row(policy, buttons, 40);
        Row(policy, Occupy(scanStatus), 34);
        policyCard.Controls.Add(policy);
        Row(settings, policyCard, 287);
        Row(settings, CodexCard(), 339);
        var storage = Bar();
        storage.Controls.Add(Action("清除本机同步数据…", delegate { return RemoveLibrary("reset", null); }));
        Row(settings, storage, 44);
        pageBody.Controls.Add(settings);
        return pageBody;
    }
    void ReadSettings(bool includePolicy)
    {
        settingFields = true;
        try
        {
            login.Checked = shell.StartOnLogin;
            startupMessage.Text = shell.StartupDisabledBySystem
                                      ? "Windows 已禁用此启动项，可在系统的「启动应用」中恢复。"
                                      : "关闭主窗口后，任务栏通知区域仍可查看同步状态。";
            if (includePolicy || previousEnabled != shell.AutoSyncEnabled)
                automatic.Checked = shell.AutoSyncEnabled;
            previousEnabled = shell.AutoSyncEnabled;
            if (includePolicy)
            {
                away.Checked = shell.AwaySmall;
                homeUrl.Text = shell.HomeUrl;
                adapter.Text = shell.HomeInterface;
            }
            scanStatus.Text =
                shell.StatusText + (shell.LastChecked.HasValue
                                        ? "  ·  " + shell.LastChecked.Value.ToLocalTime().ToString("HH:mm")
                                        : "");
        }
        finally
        {
            settingFields = false;
        }
    }
}
}
