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
    Sidebar sidebar;
    readonly Label heading = Style.Text("同步概览", 22, true, Style.Ink),
                   libEmpty = Style.Text("", 9, false, Style.Muted),
                   cacheEmpty = Style.Text("", 9, false, Style.Muted),
                   queueEmpty = Style.Text("", 9, false, Style.Muted),
                   scanStatus = Style.Text("", 9, false, Style.Muted),
                   startupMessage = Style.Text("", 9, false, Style.Muted);
    Dashboard dashboard;
    readonly FlowLayoutPanel writeTools = Bar();
    readonly CheckBox login = Check("登录 Windows 时启动"), automatic = Check("每分钟检查绑定来源"),
                      away = Check("外出时自动上传小任务");
    readonly TextBox homeUrl = new TextBox(), adapter = new TextBox();
    Button savePolicy, mergeFork, libraryTimeline, syncPending, syncSelected, syncErrors;
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
        SuspendLayout();
        Text = "心狐同步 · Moon Fox Sync";
        Icon = Icon.ExtractAssociatedIcon(Application.ExecutablePath);
        Font = Style.Type(9);
        ForeColor = Style.Ink;
        BackColor = Style.Canvas;
        StartPosition = FormStartPosition.CenterScreen;
        ClientSize = new Size(1070, 730);
        MinimumSize = new Size(890, 600);
        var layout = new TableLayoutPanel { Dock = DockStyle.Fill, Margin = Padding.Empty, ColumnCount = 2,
                                            RowCount = 1 };
        layout.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, 192));
        layout.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        layout.RowStyles.Add(new RowStyle(SizeType.Percent, 100));
        Controls.Add(layout);
        sidebar = new Sidebar(ShowSection);
        var rail = new System.Windows.Forms.Integration.ElementHost {
            Dock = DockStyle.Fill, Margin = Padding.Empty, Child = sidebar
        };
        sidebar.WidthChanged += delegate(int logicalWidth) {
            using (var g = CreateGraphics())
                layout.ColumnStyles[0].Width = logicalWidth * g.DpiX / 96f;
        };
        layout.Controls.Add(rail, 0, 0);
        var workspace = Column();
        workspace.Padding = new Padding(28, 11, 26, 10);
        var head = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 2, RowCount = 1,
                                          Margin = Padding.Empty };
        head.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        head.ColumnStyles.Add(new ColumnStyle(SizeType.AutoSize));
        head.Controls.Add(Occupy(heading), 0, 0);
        var refresh = Action("刷新", RefreshPage);
        refresh.Anchor = AnchorStyles.Right;
        refresh.Margin = Padding.Empty;
        head.Controls.Add(refresh, 1, 0);
        Row(workspace, head, 62);
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
        AutoScaleDimensions = new SizeF(96, 96);
        AutoScaleMode = AutoScaleMode.Dpi;
        ResumeLayout(true);
        var available = Screen.FromControl(this).WorkingArea.Size;
        MinimumSize = new Size(Math.Min(MinimumSize.Width, available.Width),
                               Math.Min(MinimumSize.Height, available.Height));
        Size = new Size(Math.Min(Width, available.Width), Math.Min(Height, available.Height));
        DisplayPage("overview");
    }
    void DisplayPage(string target)
    {
        page = target;
        foreach (var p in pages)
            p.Value.Visible = p.Key == target;
        heading.Text = sidebar.SelectPage(target);
        status.Text = "";
        pages[target].BringToFront();
    }
    Control Overview()
    {
        dashboard = new Dashboard(delegate(string destination) { ShowSection(destination); },
                                  delegate(string id) { SelectProject(id); },
                                  async delegate { await Run(Pair); },
                                  async delegate { await Run(RefreshConnection); },
                                  async delegate { await Run(ImportProjects); });
        return new System.Windows.Forms.Integration.ElementHost {
            Child = dashboard, Dock = DockStyle.Fill, Margin = Padding.Empty
        };
    }
    void UpdateOverview()
    {
        if (dashboard == null)
            return;
        int queued = queueKnown ? allJobs.Count(x => x.Field("state") == "queued") : -1;
        dashboard.SetStatus(connection, serverAddress, paired, libraryCountKnown,
                            libraryCountKnown ? libraries.Count.ToString() : "—",
                            queued < 0 ? "—" : queued.ToString(),
                            shell.Busy ? "检查中" : shell.AutoSyncEnabled ? "已开启" : "已暂停");
        dashboard.SetProjects(shell.Projects, shell.ProjectError, paired);
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
    }

    static Control GridCard(ListView grid, Label empty, string glyph = null, Func<string> emptyDetail = null, Button destination = null)
    {
        var list = new Surface { Padding = new Padding(12), Margin = Padding.Empty };
        var feedback = emptyDetail == null ? empty : Style.Text("", 9, false, Style.Muted);
        feedback.Dock = DockStyle.Bottom;
        feedback.Height = 28;
        list.Controls.Add(grid);
        list.Controls.Add(feedback);
        if (emptyDetail == null) return list;

        var host = new Panel { Dock = DockStyle.Fill, Margin = Padding.Empty, BackColor = Style.Canvas };
        var notice = new Surface { Dock = DockStyle.Top, Height = 92, Padding = new Padding(16, 12, 16, 12) };
        var row = new TableLayoutPanel { Dock = DockStyle.Fill, Margin = Padding.Empty, ColumnCount = 3, RowCount = 1 };
        row.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, 54));
        row.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        row.ColumnStyles.Add(new ColumnStyle(SizeType.AutoSize));
        row.RowStyles.Add(new RowStyle(SizeType.Percent, 100));
        var symbol = Style.Symbol(glyph, Style.Plum);
        symbol.BackColor = Style.Rail;
        symbol.Anchor = AnchorStyles.Left;
        symbol.Size = new Size(42, 42);
        row.Controls.Add(symbol, 0, 0);
        var words = Column();
        empty.Font = Style.Type(11, true);
        empty.ForeColor = Style.Ink;
        Row(words, Occupy(empty), 28);
        var detail = Style.Text("", 9, false, Style.Muted);
        detail.TextAlign = ContentAlignment.TopLeft;
        Row(words, Occupy(detail), -1);
        row.Controls.Add(words, 1, 0);
        if (destination != null)
        {
            destination.Anchor = AnchorStyles.Right;
            row.Controls.Add(destination, 2, 0);
        }
        notice.Controls.Add(row);
        host.Controls.Add(list);
        host.Controls.Add(notice);
        EventHandler update = delegate {
            // A populated queue can also carry a sync result. Keep those rows selectable.
            string hint = empty.Text.Length == 0 ? null : emptyDetail();
            detail.Text = hint;
            notice.Visible = hint != null;
            list.Visible = hint == null;
            feedback.Text = empty.Text;
            feedback.Visible = hint == null && empty.Text.Length != 0;
        };
        empty.TextChanged += update;
        host.VisibleChanged += update;
        update(null, EventArgs.Empty);
        return host;
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
        breadcrumb.MinimumSize = new Size(0, 30);
        Row(pageBody, Occupy(breadcrumb), 0);
        EventHandler selection = delegate {
            versionBar.Visible = breadcrumb.Visible = units.SelectedItem != null;
        };
        units.SelectedIndexChanged += selection;
        libEmpty.TextChanged += selection;
        selection(null, EventArgs.Empty);
        Row(pageBody, GridCard(files, libEmpty, "libraries", delegate {
            if (!paired) return "使用 NAS 签发的一次性配对码连接这台电脑。";
            if (units.SelectedItem == null) return "在 NAS 控制台确认此设备的资料库授权后刷新。";
            return Managed && SnapshotId == "" ? "为已绑定的资料库保存快照并上传，即可浏览版本内容。"
                : "选择其他资料库、版本或目录继续浏览。";
        }), -1);
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
        bar.Margin = new Padding(0, 0, 0, 10);
        Row(p, bar, 0);
        Row(p, GridCard(jobs, queueEmpty, "queue", delegate {
            return queueKnown && allJobs.Count == 0 && String.IsNullOrEmpty(shell.SyncResult)
                ? "在资料库保存一个文件或项目快照，即可在这里上传。" : null;
        }, Style.Action("打开资料库", delegate { ShowSection("libraries"); })), -1);
        return p;
    }
    Control CachePage()
    {
        var p = Column();
        var bar = Bar();
        var fetch = Action("查看或取回", ReadCache, true);
        var release = Action("释放选中缓存", Evict);
        fetch.Enabled = release.Enabled = false;
        cache.SelectedIndexChanged += delegate {
            var item = cache.SelectedItems.Count == 0 ? null : cache.SelectedItems[0].Tag as Record;
            fetch.Enabled = item != null;
            release.Enabled = item != null && item.Field("state") != "nas_only";
        };
        bar.Controls.Add(fetch);
        bar.Controls.Add(release);
        bar.Margin = new Padding(0, 0, 0, 10);
        Row(p, bar, 0);
        Row(p, GridCard(cache, cacheEmpty, "cache", delegate {
            return "在资料库中打开快照并取回文件，即可在这里查看或释放本机副本。";
        }, Style.Action("打开资料库", delegate { ShowSection("libraries"); })), -1);
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
        homeUrl.AccessibleName = "家庭 HTTPS 地址";
        adapter.AccessibleName = "物理网卡名称";
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
        Row(policy, buttons, 0);
        Row(policy, Occupy(scanStatus), 34);
        policyCard.Controls.Add(policy);
        Row(settings, policyCard, 287);
        Row(settings, TransferSettingsCard(), 150);
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
