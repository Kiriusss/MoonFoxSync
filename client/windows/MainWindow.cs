using System;
using System.Collections.Generic;
using System.Drawing;
using System.IO;
using System.Linq;
using System.Reflection;
using System.Threading.Tasks;
using System.Windows.Forms;

namespace XinhuSync
{
internal sealed class Choice
{
    public readonly Record Row;
    private readonly string title;
    public Choice(Record row, string title)
    {
        Row = row;
        this.title = title;
    }
    public override string ToString()
    {
        return title;
    }
}

internal sealed partial class MainWindow : Form
{
    readonly WindowsShell shell;
    readonly ComboBox units = Picker(250), versions = Picker(305);
    readonly ListView files = Grid("名称", "类型", "大小"), jobs = Grid("快照", "资料库", "状态", "采集时间"),
                      cache = Grid("文件", "资料库", "驻留状态", "大小");
    readonly TextBox label = new TextBox { Width = 154, Margin = new Padding(0, 6, 8, 0) };
    readonly Label status = Style.Text("", 9, false, Style.Muted),
                   breadcrumb = Style.Text("/", 9, false, Style.Muted);
    readonly Panel content = new Panel { Dock = DockStyle.Fill, BackColor = Style.Canvas };
    List<Record> allJobs = new List<Record>(), libraries = new List<Record>();
    string folder = "", page = "overview", deferredPage;
    bool busy, rebuilding, paired, libraryCountKnown, queueKnown;
    string connection = "正在检查连接", serverAddress = "";
    public bool OperationInProgress
    {
        get {
            return busy;
        }
    }

    public MainWindow(WindowsShell resident)
    {
        shell = resident;
        BuildWindow();
        units.SelectedIndexChanged += async delegate
        {
            if (!rebuilding)
                await Run(SelectUnit);
        };
        versions.SelectedIndexChanged += async delegate
        {
            ShowMergeAction();
            if (!rebuilding)
                await Run(async delegate {
                    folder = "";
                    await ListFiles();
                });
        };
        files.DoubleClick += async delegate
        {
            await Run(OpenEntry);
        };
        shell.Changed += OnShellChanged;
        FormClosed += delegate
        {
            shell.Changed -= OnShellChanged;
        };
        Shown += async delegate
        {
            await Run(RefreshPage, false);
        };
    }

    void OnShellChanged(object sender, EventArgs e)
    {
        if (IsDisposed || Disposing)
            return;
        if (InvokeRequired)
        {
            BeginInvoke(new Action(delegate { OnShellChanged(sender, e); }));
            return;
        }
        UpdateOverview();
        if (page == "projects")
            RenderProjects();
        if (page == "settings")
            ReadSettings(false);
        bool checkedQueue = wasChecking && !shell.Busy;
        bool syncedQueue = wasSyncing && !shell.SyncingPending;
        wasChecking = shell.Busy;
        wasSyncing = shell.SyncingPending;
        if ((syncedQueue || (checkedQueue && Visible)) && (page == "overview" || page == "queue"))
        {
            if (busy)
                refreshQueueWhenIdle = true;
            else
                RefreshAfterScan();
        }
    }

    bool wasChecking, wasSyncing, refreshQueueWhenIdle;
    async void RefreshAfterScan()
    {
        await Run(RefreshPage, false);
    }

    public async void ShowSection(string section)
    {
        if (!pages.ContainsKey(section))
            return;
        if (busy)
        {
            deferredPage = section;
            return;
        }
        DisplayPage(section);
        await Run(RefreshPage, false);
    }

    async Task RefreshPage()
    {
        if (page == "overview")
        {
            await LoadQueue();
            await RefreshConnection();
            UpdateOverview();
        }
        else if (page == "libraries")
        {
            await RefreshConnection();
            if (libraryCountKnown || !paired)
                await SelectUnit();
        }
        else if (page == "projects")
        {
            RenderProjects();
            shell.RequestProjects();
        }
        else if (page == "queue")
            await RefreshQueue();
        else if (page == "cache")
            await RefreshCache();
        else if (page == "settings")
        {
            ReadSettings(true);
            await LoadTransferLimits();
            await LoadCodexSettings();
        }
    }

    async Task RefreshConnection()
    {
        var setup = await Bridge.Call(null, "status");
        paired = String.Equals(setup.Field("paired"), "true", StringComparison.OrdinalIgnoreCase);
        serverAddress = setup.Field("serverUrl");
        libraryCountKnown = false;
        if (!paired)
        {
            connection = "尚未配对";
            SetLibraries(new List<Record>());
        }
        else
        {
            try
            {
                SetLibraries((await Bridge.Call(null, "units")).Rows());
                connection = "已连接";
                libraryCountKnown = true;
            }
            catch
            {
                connection = "连接失败";
                if (page == "libraries")
                {
                    files.Items.Clear();
                    versions.Items.Clear();
                    libEmpty.Text = "暂时无法读取梦枢天罗。";
                }
                throw;
            }
            finally
            {
                UpdateOverview();
            }
        }
        UpdateOverview();
    }

    void SetLibraries(List<Record> data)
    {
        libraries = data;
        var old = units.SelectedItem as Choice;
        rebuilding = true;
        try
        {
            Fill(units,
                 data.Select(r => new Choice(r, r.Field("name") +
                                                    (r.Field("kind") == "external" ? " · 外部只读" : ""))),
                 "unitId", old == null ? "" : old.Row.Field("unitId"));
        }
        finally
        {
            rebuilding = false;
        }
    }
    async Task Run(Func<Task> action, bool dialog = true)
    {
        if (busy)
            return;
        busy = true;
        content.Enabled = false;
        UseWaitCursor = true;
        status.Text = "正在处理…";
        try
        {
            await action();
            if (status.Text == "正在处理…")
                status.Text = "就绪";
        }
        catch (Exception ex)
        {
            status.Text = ex.Message;
            if (dialog)
                MessageBox.Show(this, ex.Message, "心狐同步", MessageBoxButtons.OK, MessageBoxIcon.Information);
        }
        finally
        {
            UseWaitCursor = false;
            content.Enabled = true;
            busy = false;
            bool refresh = refreshQueueWhenIdle;
            refreshQueueWhenIdle = false;
            if (deferredPage != null)
            {
                string next = deferredPage;
                deferredPage = null;
                BeginInvoke(new Action(delegate { ShowSection(next); }));
            }
            else if (refresh && (page == "overview" || page == "queue"))
                BeginInvoke(new Action(RefreshAfterScan));
        }
    }
    Record Unit()
    {
        var item = units.SelectedItem as Choice;
        if (item == null)
            throw new InvalidOperationException("请先配对并选择资料库。");
        return item.Row;
    }
    string UnitId
    {
        get {
            return Unit().Field("unitId");
        }
    }
    string SnapshotId
    {
        get {
            var v = versions.SelectedItem as Choice;
            return v == null ? "" : v.Row.Field("snapshotId");
        }
    }
    bool Managed
    {
        get {
            return Unit().Field("kind") == "managed";
        }
    }
    void Writer()
    {
        if (!Managed || Unit().Field("role") != "sync")
            throw new InvalidOperationException("此操作需要受管资料库的同步权限。");
    }
    static Record Selected(ListView grid)
    {
        if (grid.SelectedItems.Count == 0)
            throw new InvalidOperationException("请先选中一行。");
        return (Record)grid.SelectedItems[0].Tag;
    }
    static void Add(ListView grid, Record row, params string[] cells)
    {
        grid.Items.Add(new ListViewItem(cells) { Tag = row });
    }
    static void Fill(ComboBox combo, IEnumerable<Choice> entries, string field, string prior)
    {
        combo.Items.Clear();
        Choice selected = null;
        foreach (var item in entries)
        {
            combo.Items.Add(item);
            if (prior != "" && item.Row.Field(field) == prior)
                selected = item;
        }
        if (selected != null)
            combo.SelectedItem = selected;
        else if (combo.Items.Count > 0)
            combo.SelectedIndex = 0;
    }
    static string VersionTitle(Record r)
    {
        var label = r.Field("label");
        if (label == "")
            label = "自动快照";
        return String.Format("{0}  {1}  {2}", label, r.Field("capturedAt"),
                             r.Field("outcome") == "fork" ? "分叉" : "");
    }
    async Task Pair()
    {
        using (var form = new PairForm())
        {
            if (form.ShowDialog(this) != DialogResult.OK)
                return;
            var args = new List<string> { "pair", "--server", form.Address, "--name", "心狐同步 · Windows" };
            if (form.CA != "")
            {
                args.Add("--ca");
                args.Add(form.CA);
            }
            await Bridge.Call(form.Code, args.ToArray());
        }
        await RefreshUnits();
        status.Text = "设备配对成功";
    }
    async Task RefreshUnits()
    {
        await RefreshConnection();
        if (page == "libraries" && libraryCountKnown)
            await SelectUnit();
    }
    async Task SelectUnit()
    {
        folder = "";
        files.Items.Clear();
        rebuilding = true;
        try
        {
            versions.Items.Clear();
            breadcrumb.Text = "/";
            if (units.SelectedItem != null && Managed)
            {
                var values = (await Bridge.Call(null, "history", UnitId)).Rows();
                Fill(versions, values.Select(r => new Choice(r, VersionTitle(r))), "snapshotId",
                     Unit().Field("head"));
            }
        }
        finally
        {
            rebuilding = false;
        }
        bool selected = units.SelectedItem != null;
        bool writer = selected && Managed && Unit().Field("role") == "sync";
        ShowMergeAction();
        versions.Enabled = selected && Managed;
        writeTools.Visible = writer;
        libraryTimeline.Visible = selected && Managed;
        if (!selected)
        {
            libEmpty.Text = paired ? "暂无已授权的资料库" : "配对后可浏览资料库";
            return;
        }
        await ListFiles();
    }
    async Task ListFiles()
    {
        files.Items.Clear();
        breadcrumb.Text = folder == "" ? "/" : "/" + folder;
        libEmpty.Text = "";
        if (Managed && SnapshotId == "")
        {
            libEmpty.Text = "资料库还没有已发布版本";
            return;
        }
        var args = new List<string> { "ls", UnitId, "--path", folder };
        if (Managed)
        {
            args.Add("--snapshot");
            args.Add(SnapshotId);
        }
        var data = (await Bridge.Call(null, args.ToArray())).Rows();
        foreach (var r in data)
        {
            var kind = r.Field("kind");
            Add(files, r, Path.GetFileName(r.Field("path")),
                kind == "directory" ? "目录"
                : kind == "file"    ? "文件"
                                    : "链接",
                FormatBytes(r.Field("size")));
        }
        libEmpty.Text = data.Count == 0 ? "空目录" : "";
        status.Text = String.Format("{0} 项", data.Count);
    }
    async Task Up()
    {
        int at = folder.LastIndexOf('/');
        folder = at < 0 ? "" : folder.Substring(0, at);
        await ListFiles();
    }
    bool CanMergeSelected()
    {
        var revision = versions.SelectedItem as Choice;
        return units.SelectedItem != null && Unit().Field("role") == "sync" && Managed && revision != null &&
               revision.Row.Field("outcome") == "fork";
    }
    void ShowMergeAction()
    {
        if (mergeFork != null)
            mergeFork.Visible = CanMergeSelected();
    }
    async Task MergeSelectedVersion()
    {
        if (!CanMergeSelected())
            throw new InvalidOperationException("请选择有同步权限的资料库中的分叉版本。");
        await ShowMerger(UnitId, Unit().Field("name"), SnapshotId);
    }
    async Task ShowMerger(string unit, string title, string fork)
    {
        using (var merger = new MergeWorkflow(title, unit, fork))
        {
            if (merger.ShowDialog(this) != DialogResult.OK || String.IsNullOrEmpty(merger.Published))
                return;
            string message = "已发布 NAS 合并快照 " + merger.Published;
            try
            {
                if (page == "libraries")
                    await RefreshUnits();
                await shell.RefreshProjects(false);
                if (page == "projects")
                    RenderProjects();
            }
            catch (Exception ex)
            {
                message += " · 请稍后刷新视图：" + ex.Message;
            }
            status.Text = message;
        }
    }
    async Task OpenEntry()
    {
        Record r = Selected(files);
        string path = r.Field("path"), kind = r.Field("kind");
        if (kind == "directory")
        {
            folder = path;
            await ListFiles();
            return;
        }
        if (kind != "file")
            throw new InvalidOperationException("此目录项无法作为普通文件读取。");
        if (Managed)
        {
            var local = await Bridge.Call(null, "cache", "fetch", UnitId, SnapshotId, "--path", path);
            ViewFile.Show(this, local.Field("localPath"), path);
            status.Text = "已校验并保存到下载缓存";
        }
        else
            using (var d = new SaveFileDialog { Title = "下载到新文件", FileName = Path.GetFileName(path),
                                                OverwritePrompt = true })
            {
                if (d.ShowDialog(this) != DialogResult.OK)
                    return;
                var saved = await Bridge.Call(null, "get", UnitId, "--path", path, "--to", d.FileName);
                ViewFile.Show(this, saved.Field("path"), path);
                status.Text = "已校验下载文件";
            }
    }
    async Task Bind(bool single)
    {
        Writer();
        string path;
        if (single)
        {
            using (var d = new OpenFileDialog { Title = "选择要同步的文件" })
            {
                if (d.ShowDialog(this) != DialogResult.OK)
                    return;
                path = d.FileName;
            }
        }
        else
        {
            using (var d = new FolderBrowserDialog { Description = "选择要作为整体同步的项目目录",
                                                     ShowNewFolderButton = false })
            {
                if (d.ShowDialog(this) != DialogResult.OK)
                    return;
                path = d.SelectedPath;
            }
        }
        var args = new List<string> {
            "unit", "bind", UnitId, "--path", path, "--profile", single ? "file" : "project"
        };
        var preview = await Bridge.Call(null, args.Concat(new[] { "--preview" }).ToArray());
        string previous = preview.Field("baseSnapshotId");
        string message =
            path + "\n\n" + (previous == "" ? "建立初始版本。" : "下一份快照接续基线 " + previous + "。");
        if (MessageBox.Show(this, message, "确认绑定至 " + Unit().Field("name"), MessageBoxButtons.OKCancel) !=
            DialogResult.OK)
            return;
        if (previous != "")
        {
            args.Add("--base");
            args.Add(previous);
        }
        await Bridge.Call(null, args.ToArray());
        status.Text = "已绑定 " + path;
    }
    async Task LoadQueue()
    {
        allJobs = (await Bridge.Call(null, "queue")).Rows();
        allJobs.Reverse();
        queueKnown = true;
        UpdateOverview();
    }
    async Task RefreshQueue()
    {
        await LoadQueue();
        jobs.Items.Clear();
        foreach (var r in allJobs)
            Add(jobs, r, JobTitle(r), LibraryName(r.Field("unitId")), StateText(r.Field("state")),
                LocalTime(r.Field("capturedAt")));
    }
    async Task CaptureVersion()
    {
        Writer();
        string name = label.Text.Trim();
        var args = new List<string> { "snapshot", UnitId };
        if (name != "")
        {
            args.Add("--name");
            args.Add(name);
        }
        var v = await Bridge.Call(null, args.ToArray());
        DisplayPage("queue");
        await RefreshQueue();
        foreach (ListViewItem item in jobs.Items)
            if (((Record)item.Tag).Field("snapshotId") == v.Field("snapshotId"))
            {
                item.Selected = true;
                item.EnsureVisible();
            }
        status.Text = "本地版本已就绪 · " + v.Field("snapshotId");
    }
    async Task SyncJob()
    {
        var v = Selected(jobs);
        if (v.Field("state") != "queued")
            throw new InvalidOperationException("请选择一条待上传的快照。");
        var ack =
            await Bridge.Call(null, "sync", v.Field("unitId"), "--snapshot", v.Field("snapshotId"), "--now");
        await RefreshQueue();
        status.Text = ack.Field("outcome") == "fork" ? "NAS 已保存并发分叉" : "NAS 已保存此版本";
    }
    async Task RefreshCache()
    {
        cache.Items.Clear();
        foreach (var r in (await Bridge.Call(null, "cache", "list")).Rows())
            Add(cache, r, r.Field("path"), LibraryName(r.Field("unitId")),
                r.Field("state") == "nas_only"     ? "仅 NAS"
                : r.Field("state") == "downloaded" ? "已下载"
                                                   : r.Field("state"),
                FormatBytes(r.Field("size")));
        cacheEmpty.Text = cache.Items.Count == 0 ? "还没有下载缓存" : "";
        status.Text = String.Format("{0} 条下载缓存记录", cache.Items.Count);
    }
    async Task ReadCache()
    {
        var item = Selected(cache);
        var v = await Bridge.Call(null, "cache", "fetch", item.Field("unitId"), item.Field("snapshotId"),
                                  "--path", item.Field("path"));
        ViewFile.Show(this, v.Field("localPath"), item.Field("path"));
        await RefreshCache();
    }
    async Task Evict()
    {
        var item = Selected(cache);
        if (MessageBox.Show(this, "确认将此版本的应用下载缓存释放为仅 NAS？\n" + item.Field("path"),
                            "释放下载缓存", MessageBoxButtons.OKCancel) != DialogResult.OK)
            return;
        await Bridge.Call(null, "cache", "evict", item.Field("unitId"), item.Field("snapshotId"), "--path",
                          item.Field("path"));
        await RefreshCache();
        status.Text = "已释放应用下载缓存";
    }

    [STAThread]
    public static void Main(string[] args)
    {
        WindowsShell.Run(args);
    }
}
}
