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
internal sealed partial class WindowsShell
{
    readonly Timer transferPoll = new Timer { Interval = 500 };
    readonly Timer foxAnimation = new Timer { Interval = 50 };
    readonly List<Icon> foxFrames = new List<Icon>();
    Icon restingFox;
    int foxFrame;
    bool readingTransfers;
    public List<Record> Transfers { get; private set; }
    public event EventHandler TransfersChanged;
    string transferFolder;
    public string TransferError { get; private set; }

    static Icon FoxIcon(string name)
    {
        using (var stream = Assembly.GetExecutingAssembly().GetManifestResourceStream(name + ".ico"))
        {
            if (stream == null) return null;
            using (var icon = new Icon(stream, SystemInformation.SmallIconSize)) return (Icon)icon.Clone();
        }
    }

    void StartTransfers()
    {
        string home = Environment.GetEnvironmentVariable("KDRIVE_HOME");
        if (String.IsNullOrEmpty(home)) home = Path.Combine(
            Environment.GetFolderPath(Environment.SpecialFolder.ApplicationData), "XinhuSync");
        transferFolder = Path.Combine(home,"transfers");
        Transfers = new List<Record>();
        TransferError = "";
        restingFox = FoxIcon("TrayFoxIdle");
        for (var i = 0; i < 8; i++)
        {
            var image = FoxIcon("TrayFoxRun" + i);
            if (image != null) foxFrames.Add(image);
        }
        if (restingFox != null) tray.Icon = restingFox;
        foxAnimation.Tick += delegate {
            foxFrame = (foxFrame + 1) % foxFrames.Count;
            tray.Icon = foxFrames[foxFrame];
        };
        transferPoll.Tick += async delegate { await ReadTransfers(); };
        transferPoll.Start();
    }

    async Task ReadTransfers()
    {
        if (readingTransfers || quitting) return;
        readingTransfers = true;
        var rows = new List<Record>();
        string error = "";
        try
        {
            if (Directory.Exists(transferFolder) && Directory.EnumerateFiles(transferFolder, "*.json").Any())
                rows = (await Bridge.Call(null,"transfers")).Rows();
        }
        catch (Exception ex) { error = ex.Message; }
        finally { readingTransfers = false; }
        if (quitting || (rows.Count == 0 && Transfers.Count == 0 && error == TransferError)) return;
        Transfers = rows;
        TransferError = error;
        bool animate = rows.Any(r => r.Field("phase") == "transferring") && foxFrames.Count > 0;
        if (animate && !foxAnimation.Enabled)
        {
            foxFrame = 0; tray.Icon = foxFrames[0]; foxAnimation.Start();
        }
        else if (!animate && foxAnimation.Enabled)
        {
            foxAnimation.Stop(); tray.Icon = restingFox ?? window.Icon ?? SystemIcons.Application;
        }
        UpdateTray();
        if (TransfersChanged != null) TransfersChanged(this, EventArgs.Empty);
    }

    void DisposeTransfers()
    {
        transferPoll.Dispose();
        foxAnimation.Dispose();
        if (restingFox != null) restingFox.Dispose();
        foreach (var icon in foxFrames) icon.Dispose();
    }

    internal static string TransferPhase(Record row)
    {
        if (row.Field("phase") == "preparing") return "准备传输";
        if (row.Field("phase") == "finalizing") return row.Field("direction") == "upload" ? "正在提交" : "正在完成";
        return row.Field("direction") == "upload" ? "正在上传" : "正在下载";
    }
    internal static string FileProgress(Record row)
    {
        long done, total;
        if (!Int64.TryParse(row.Field("fileBytes"),out done) || !Int64.TryParse(row.Field("fileTotal"),out total)
            || String.IsNullOrEmpty(row.Field("path"))) return "";
        return total <= 0 ? "0 B" : String.Format("{0:P0} · {1} / {2}", Math.Min(1d,(double)done / total),
                MainWindow.FormatBytes(done.ToString()), MainWindow.FormatBytes(total.ToString()));
    }
    internal static string Rate(Record row)
    {
        if (row.Field("phase") != "transferring") return "";
        var raw = row.Field("bytesPerSecond");
        return MainWindow.FormatBytes(String.IsNullOrEmpty(raw) ? "0" : raw) + "/s";
    }
}

internal sealed partial class MainWindow
{
    readonly ListView liveGrid = Grid("当前文件", "传输", "速度", "文件进度");
    RowStyle liveRow, projectRow;
    readonly Panel projectTransfers = new Panel { Dock = DockStyle.Fill, AutoScroll = true, Margin = Padding.Empty };


    void AddTransferPanel(TableLayoutPanel workspace)
    {
        Row(workspace, projectTransfers, 1);
        projectRow = workspace.RowStyles[workspace.RowCount - 1];
        projectRow.Height = 0;
        Row(workspace, liveGrid, 1);
        liveRow = workspace.RowStyles[workspace.RowCount - 1];
        liveRow.Height = 0;
        liveGrid.AccessibleName = "当前传输";
        liveGrid.ShowItemToolTips = true;
        liveGrid.Visible = false;
        shell.TransfersChanged += RenderTransfers;
        FormClosed += delegate { shell.TransfersChanged -= RenderTransfers; };
    }
    void RenderTransfers(object sender, EventArgs args)
    {
        if (IsDisposed || Disposing) return;
        var rows = shell.Transfers;
        var groups = ProjectTransferProgress.Group(shell.Transfers);
        foreach (var group in groups)
        {
            var panel = projectTransfers.Controls[group.Key] as ProjectProgressPanel;
            if (panel == null)
            {
                panel = new ProjectProgressPanel { Name = group.Key };
                projectTransfers.Controls.Add(panel);
            }
            string unit = group.Row.Field("unitId");
            var project = shell.Projects.FirstOrDefault(p => p.Field("unitId") == unit);
            panel.UpdateProgress(group, project == null ? LibraryName(unit) : project.Field("name"));
        }
        var groupKeys = new HashSet<string>(groups.Select(g => g.Key));
        foreach (Control c in projectTransfers.Controls.Cast<Control>().ToArray())
            if (!groupKeys.Contains(c.Name)) c.Dispose();
        projectRow.Height = Math.Min(3, groups.Count) * 36;
        rows = rows.Where(r => !String.IsNullOrEmpty(r.Field("path"))).ToList();
        var error = shell.TransferError;
        bool visible = rows.Count > 0 || !String.IsNullOrEmpty(error);
        liveGrid.BeginUpdate();
        try
        {
            var present = new HashSet<string>(StringComparer.Ordinal);
            foreach (var row in rows)
            {
                string id = row.Field("id"), path = row.Field("path"), phase = WindowsShell.TransferPhase(row);
                present.Add(id);
                var values = new[] { path.Length > 0 ? path : phase, phase, WindowsShell.Rate(row), WindowsShell.FileProgress(row) };
                var item = liveGrid.Items[id];
                if (item == null)
                    liveGrid.Items.Add(new ListViewItem(values) { Name = id, ToolTipText = path });
                else
                {
                    for (int i = 0; i < values.Length; i++)
                        if (item.SubItems[i].Text != values[i]) item.SubItems[i].Text = values[i];
                    item.ToolTipText = path;
                }
            }
            if (!String.IsNullOrEmpty(error))
            {
                const string errorKey = "@transfer-error";
                present.Add(errorKey);
                var item = liveGrid.Items[errorKey] ?? liveGrid.Items.Add(errorKey, "", -1);
                item.Text = "读取传输状态失败：" + error;
            }
            for (int i = liveGrid.Items.Count - 1; i >= 0; i--)
                if (!present.Contains(liveGrid.Items[i].Name)) liveGrid.Items.RemoveAt(i);
            liveGrid.Visible = visible;
            liveRow.Height = visible ? 35 + Math.Min(6,Math.Max(1,liveGrid.Items.Count)) * (Font.Height + 6) : 0;
        }
        finally { liveGrid.EndUpdate(); }
    }
}
}
