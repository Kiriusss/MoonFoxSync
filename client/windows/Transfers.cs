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
        if (row.Field("phase") == "finalizing") return "正在完成";
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
    RowStyle liveRow;

    void AddTransferPanel(TableLayoutPanel workspace)
    {
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
        var error = shell.TransferError;
        bool visible = rows.Count > 0 || !String.IsNullOrEmpty(error);
        liveGrid.BeginUpdate();
        try
        {
            liveGrid.Items.Clear();
            foreach (var row in rows)
            {
                var path = row.Field("path");
                var phase = WindowsShell.TransferPhase(row);
                var item = new ListViewItem(new[] {path.Length > 0 ? path : phase, phase, WindowsShell.Rate(row),
                                                   WindowsShell.FileProgress(row)}) { ToolTipText = path };
                liveGrid.Items.Add(item);
            }
            if (!String.IsNullOrEmpty(error)) liveGrid.Items.Add("读取传输状态失败：" + error);
            liveGrid.Visible = visible;
            liveRow.Height = visible ? 35 + Math.Min(4,Math.Max(1,rows.Count)) * (Font.Height + 6) : 0;
        }
        finally { liveGrid.EndUpdate(); }
    }
}
}
