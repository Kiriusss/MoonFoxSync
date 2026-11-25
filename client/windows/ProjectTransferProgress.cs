using System;
using System.Collections.Generic;
using System.Drawing;
using System.Linq;
using System.Windows.Forms;

namespace XinhuSync
{
internal sealed class ProjectTransferProgress
{
    public readonly string Key;
    public readonly Record Row;
    public readonly bool KnownTotal;
    public readonly int BarValue;
    public readonly string Caption;

    ProjectTransferProgress(string key, Record row, IEnumerable<Record> rows)
    {
        Key = key;
        Row = row;
        var parts = new List<string> { WindowsShell.TransferPhase(row) };
        long done, total;
        if (row.Field("direction") == "upload")
        {
            if (Int64.TryParse(row.Field("projectTotal"), out total) && total >= 0 &&
                Int64.TryParse(row.Field("projectBytes"), out done) && done >= 0)
            {
                KnownTotal = true;
                BarValue = total > 0 ? (int)Math.Min(1000, 1000d * done / total) : (row.Field("phase") == "finalizing" ? 1000 : 0);
                if (total > 0) parts.Add(String.Format("{0:P0}", BarValue / 1000d));
                parts.Add(MainWindow.FormatBytes(done.ToString()) + " / " + MainWindow.FormatBytes(total.ToString()));
            }
            else if (row.Field("phase") != "finalizing")
                parts.Add(row.Field("phase") == "preparing" ? "正在统计项目总量…" : "项目总量尚未提供");
            if (row.Field("projectRate") != "")
                parts.Add(MainWindow.FormatBytes(row.Field("projectRate")) + "/s");
        }
        else
        {
            var files = rows.Where(r => r.Field("path") != "").ToList();
            if (files.Count > 0)
            {
                long rate = files.Sum(r => { long value; return Int64.TryParse(r.Field("bytesPerSecond"), out value) ? Math.Max(0, value) : 0L; });
                parts.Add(files.Count + " 个文件");
                parts.Add(MainWindow.FormatBytes(rate.ToString()) + "/s");
            }
        }
        Caption = String.Join(" · ", parts);
    }

    public static List<ProjectTransferProgress> Group(IEnumerable<Record> rows)
    {
        return rows.GroupBy(row => {
            string id = row.Field("operationId");
            if (id.Length == 0) id = row.Field("snapshotId");
            if (id.Length == 0) id = row.Field("id");
            return row.Field("direction") + "/" + row.Field("unitId") + "/" + id;
        }).OrderBy(g => g.Key, StringComparer.Ordinal)
        .Select(g => new ProjectTransferProgress(g.Key, g.FirstOrDefault(r => r.Field("phase") == "finalizing")
                        ?? g.FirstOrDefault(r => r.Field("projectTotal") != "") ?? g.First(), g)).ToList();
    }

    public void Apply(ProgressBar bar)
    {
        bar.Visible = Row.Field("direction") == "upload";
        bar.Style = KnownTotal ? ProgressBarStyle.Continuous : ProgressBarStyle.Marquee;
        if (KnownTotal) bar.Value = BarValue;
    }
}

internal sealed class ProjectProgressPanel : Panel
{
    readonly Label title = new Label { Dock = DockStyle.Fill, AutoEllipsis = true, TextAlign = ContentAlignment.MiddleLeft };
    readonly ProgressBar bar = new ProgressBar { Dock = DockStyle.Bottom, Height = 8, Maximum = 1000, AccessibleName = "项目总进度" };
    public ProjectProgressPanel()
    {
        Height = 36;
        Dock = DockStyle.Top;
        Padding = new Padding(4, 0, 4, 4);
        Controls.Add(title);
        Controls.Add(bar);
    }
    public void UpdateProgress(ProjectTransferProgress progress, string project)
    {
        title.Text = project + " · " + progress.Caption;
        progress.Apply(bar);
    }
}
}
