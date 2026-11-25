using System;
using System.Drawing;
using System.Windows.Forms;

namespace XinhuSync
{
internal sealed class TrayPanel : Form
{
    readonly WindowsShell shell;
    readonly Label summary, connection, counts;
    readonly Button check, pause, sync;
    readonly Button[] projects = new Button[3];
    readonly ToolTip projectTips = new ToolTip();
    readonly Image portrait;
    readonly Color accent = Color.FromArgb(141, 55, 87);
    readonly Color muted = Color.FromArgb(101, 92, 94);

    public TrayPanel(WindowsShell owner)
    {
        shell = owner;
        Text = "心狐同步";
        ShowInTaskbar = false;
        FormBorderStyle = FormBorderStyle.FixedToolWindow;
        ControlBox = false;
        Text = "";
        TopMost = true;
        StartPosition = FormStartPosition.Manual;
        BackColor = Color.FromArgb(255, 253, 251);
        Font = new Font("Microsoft YaHei UI", 9F);
        AutoScaleDimensions = new SizeF(96, 96);
        AutoScaleMode = AutoScaleMode.Dpi;
        ClientSize = new Size(375, 363);
        Padding = new Padding(16, 14, 16, 12);
        var content = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 2, RowCount = 10 };
        content.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 50));
        content.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 50));
        foreach (var height in new[] { 29, 23, 26, 35, 30, 30, 30, 40, 39, 34 })
            content.RowStyles.Add(new RowStyle(SizeType.Absolute, height));
        Controls.Add(content);

        var header =
            new FlowLayoutPanel { Dock = DockStyle.Fill, Margin = Padding.Empty, WrapContents = false };
        using (var asset = typeof(TrayPanel).Assembly.GetManifestResourceStream("xinhu64.png"))
        {
            if (asset != null)
            {
                using (var source = Image.FromStream(asset)) portrait = new Bitmap(source);
                header.Controls.Add(new PictureBox { Image = portrait, Width = 26, Height = 26,
                                                     SizeMode = PictureBoxSizeMode.Zoom,
                                                     Margin = new Padding(0, 0, 7, 0) });
            }
        }
        header.Controls.Add(new Label { Text = "心狐同步", AutoSize = true, Margin = new Padding(0, 1, 0, 0),
                                        Font = new Font(Font.FontFamily, 12F, FontStyle.Bold),
                                        ForeColor = Color.FromArgb(52, 42, 46) });
        content.Controls.Add(header, 0, 0);
        content.SetColumnSpan(header, 2);
        connection = Line(content, 1, "", true);
        summary = Line(content, 2, "", false);
        var queueRow = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 2, RowCount = 1,
                                             Margin = Padding.Empty };
        queueRow.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        queueRow.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, 92));
        content.Controls.Add(queueRow, 0, 3);
        content.SetColumnSpan(queueRow, 2);
        counts = Line(queueRow, 0, "", true);
        queueRow.SetColumnSpan(counts, 1);
        sync = Action(queueRow, 1, 0, "立即同步", async delegate { await shell.SyncPending(); });
        sync.BackColor = accent;
        sync.ForeColor = Color.White;
        for (int i = 0; i < projects.Length; i++)
        {
            var b = Action(content, 0, i + 4, "", delegate(object sender, EventArgs e) {
                var id = ((Button)sender).Tag as string;
                if (!String.IsNullOrEmpty(id))
                    shell.OpenProject(id);
            });
            b.TextAlign = ContentAlignment.MiddleLeft;
            b.AutoEllipsis = true;
            b.Margin = new Padding(0, 1, 0, 1);
            content.SetColumnSpan(b, 2);
            projects[i] = b;
        }
        check = Action(content, 0, 7, "立即检查", async delegate { await shell.CheckNow(); });
        pause = Action(content, 1, 7, "", delegate { shell.ToggleAuto(); });
        Action(content, 0, 8, "打开主窗口", delegate { shell.ShowWindow("overview"); });
        Action(content, 1, 8, "打开队列", delegate { shell.ShowWindow("queue"); });
        Action(content, 0, 9, "设置", delegate { shell.ShowWindow("settings"); });
        Action(content, 1, 9, "退出", delegate { shell.Quit(); });
        Deactivate += delegate
        {
            Hide();
        };
        KeyPreview = true;
        KeyDown += delegate(object sender, KeyEventArgs args)
        {
            if (args.KeyCode == Keys.Escape)
                Hide();
        };
        UpdateValues();
    }

    Label Line(TableLayoutPanel grid, int row, string text, bool secondary)
    {
        var line =
            new Label { Text = text, ForeColor = secondary ? muted : Color.FromArgb(52, 42, 46),
                        AutoEllipsis = true, Dock = DockStyle.Fill, TextAlign = ContentAlignment.MiddleLeft };
        grid.Controls.Add(line, 0, row);
        grid.SetColumnSpan(line, 2);
        return line;
    }

    Button Action(TableLayoutPanel grid, int column, int row, string text, EventHandler action)
    {
        var button = new Button {
            Text = text,           FlatStyle = FlatStyle.Flat,
            Dock = DockStyle.Fill, BackColor = Color.FromArgb(246, 240, 241),
            ForeColor = accent,    Margin = new Padding(column == 0 ? 0 : 4, 3, column == 0 ? 4 : 0, 3)
        };
        button.FlatAppearance.BorderSize = 0;
        button.Click += action;
        grid.Controls.Add(button, column, row);
        return button;
    }

    protected override void OnFormClosing(FormClosingEventArgs e)
    {
        if (e.CloseReason == CloseReason.UserClosing)
        {
            e.Cancel = true;
            Hide();
        }
        base.OnFormClosing(e);
    }

    protected override void Dispose(bool disposing)
    {
        if (disposing)
            projectTips.Dispose();
        if (disposing && portrait != null)
            portrait.Dispose();
        base.Dispose(disposing);
    }

    public void UpdateValues()
    {
        for (int i = 0; i < projects.Length; i++)
        {
            var b = projects[i];
            var p = i < shell.Projects.Count ? shell.Projects[i] : null;
            b.Tag = p == null ? null : p.Field("unitId");
            b.Text =
                p == null
                    ? (i == 0 ? (String.IsNullOrEmpty(shell.ProjectError) ? "暂无近期项目" : "项目状态暂不可用")
                              : "")
                    : p.Field("name") + "     " + ProjectDisplay.Nas(p) + "   ·   Git " + ProjectDisplay.Git(p);
            projectTips.SetToolTip(b, b.Text);
            b.Enabled = p != null;
            b.Visible = p != null || i == 0;
        }
        summary.Text = !String.IsNullOrEmpty(shell.SyncResult) && (shell.SyncingPending || !shell.Busy)
            ? shell.SyncResult : shell.StatusText;
        projectTips.SetToolTip(summary, String.IsNullOrEmpty(shell.SyncErrorDetails)
            ? summary.Text : summary.Text + Environment.NewLine + shell.SyncErrorDetails);
        connection.Text = shell.Connection;
        counts.Text = shell.Queued < 0 ? "队列待读取" : "待上传 " + shell.Queued + " 项";
        projectTips.SetToolTip(counts, counts.Text +
            (shell.LastChecked.HasValue ? " · 上次检查 " + shell.LastChecked.Value.ToString("HH:mm") : ""));
        sync.Text = shell.SyncButtonText;
        sync.Enabled = shell.CanSyncPending;
        if (shell.Transfers != null && shell.Transfers.Count > 0)
        {
            var active = shell.Transfers[0];
            summary.Text = WindowsShell.TransferPhase(active) + " · " + WindowsShell.Rate(active) + " " + active.Field("path");
            var progress = WindowsShell.FileProgress(active);
            if (!String.IsNullOrEmpty(progress))
                counts.Text += " · " + progress;
            projectTips.SetToolTip(summary, summary.Text);
            projectTips.SetToolTip(counts, counts.Text);
        }
        pause.Text = shell.AutoSyncEnabled ? "暂停自动检查" : "启用自动检查";
        check.Enabled = !shell.Busy;
        AccessibleName = "心狐同步快捷面板";
    }

    public void ShowAt(Point anchor)
    {
        var bounds = Screen.FromPoint(anchor).WorkingArea;
        int x =
            Math.Min(Math.Max(bounds.Left, anchor.X - Width / 2), Math.Max(bounds.Left, bounds.Right - Width));
        int y = anchor.Y - Height - 8;
        if (y < bounds.Top)
            y = anchor.Y + 8;
        y = Math.Min(Math.Max(bounds.Top, y), Math.Max(bounds.Top, bounds.Bottom - Height));
        Location = new Point(x, y);
        UpdateValues();
        Show();
        BringToFront();
        Activate();
    }
}
}
