using System;
using System.Drawing;
using System.Drawing.Drawing2D;
using System.Linq;
using System.Windows.Forms;

namespace XinhuSync
{
internal sealed class TrayPanel : Form
{
    readonly WindowsShell shell;
    readonly TableLayoutPanel layout = new TableLayoutPanel();
    readonly Label connection, summary, schedule;
    readonly Button queue, check, pause, sync;
    readonly ProgressBar projectBar;
    readonly Panel scheduleDot;
    readonly ProjectButton[] projects = new ProjectButton[3];
    readonly ToolTip tips = new ToolTip();
    readonly Image portrait;
    Point anchor;

    static Color Paper { get { return SystemInformation.HighContrast ? SystemColors.Window : Style.Paper; } }
    static Color Ink { get { return SystemInformation.HighContrast ? SystemColors.WindowText : Style.Ink; } }
    static Color Muted { get { return SystemInformation.HighContrast ? SystemColors.WindowText : Style.Muted; } }
    static Color Tint { get { return SystemInformation.HighContrast ? SystemColors.Control : Style.Blush; } }
    static Color Accent { get { return SystemInformation.HighContrast ? SystemColors.Highlight : Style.Plum; } }

    public TrayPanel(WindowsShell owner)
    {
        shell = owner;
        SuspendLayout();
        Text = "心狐同步";
        AccessibleName = "心狐同步快捷面板";
        ShowInTaskbar = false;
        FormBorderStyle = FormBorderStyle.None;
        TopMost = true;
        StartPosition = FormStartPosition.Manual;
        BackColor = Paper;
        Font = Style.Type(9);
        DoubleBuffered = true;
        AutoScaleDimensions = new SizeF(96, 96);
        AutoScaleMode = AutoScaleMode.Dpi;
        Padding = new Padding(16, 12, 16, 12);
        ClientSize = new Size(364, 380);
        layout.Dock = DockStyle.Fill;
        layout.ColumnCount = 1;
        layout.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        foreach (int height in new[] { 54, 52, 30, 38, 38, 38, 42, 1, 42 })
            layout.RowStyles.Add(new RowStyle(SizeType.Absolute, height));
        layout.RowCount = layout.RowStyles.Count;
        Controls.Add(layout);

        var header = Row(48, 0, 32);
        using (var asset = typeof(TrayPanel).Assembly.GetManifestResourceStream("xinhu64.png"))
        {
            if (asset != null)
            {
                using (var source = Image.FromStream(asset)) portrait = new Bitmap(source);
                header.Controls.Add(new PictureBox { Image = portrait, Dock = DockStyle.Fill,
                    SizeMode = PictureBoxSizeMode.Zoom, Margin = new Padding(0, 4, 7, 7) }, 0, 0);
            }
        }
        var title = new TableLayoutPanel { Dock = DockStyle.Fill, Margin = Padding.Empty, RowCount = 2 };
        title.RowStyles.Add(new RowStyle(SizeType.Percent, 54));
        title.RowStyles.Add(new RowStyle(SizeType.Percent, 46));
        title.Controls.Add(Caption("心狐同步", 12, true, Ink), 0, 0);
        connection = Caption("", 8.5F, false, Muted);
        connection.TextAlign = ContentAlignment.TopLeft;
        title.Controls.Add(connection, 0, 1);
        header.Controls.Add(title, 1, 0);
        header.Controls.Add(IconAction("settings", "打开设置", delegate { shell.ShowWindow("settings"); }), 2, 0);
        layout.Controls.Add(header, 0, 0);

        var transfer = Row(0, 89);
        transfer.RowStyles.Clear();
        transfer.RowCount = 2;
        transfer.Padding = new Padding(8, 5, 8, 5);
        transfer.RowStyles.Add(new RowStyle(SizeType.Absolute, 35));
        transfer.RowStyles.Add(new RowStyle(SizeType.Percent, 100));
        transfer.BackColor = Tint;
        transfer.SizeChanged += delegate {
            if (transfer.Width > 0 && transfer.Height > 0)
                using (var outline = Style.Rounded(transfer.ClientRectangle, Math.Max(2, transfer.Padding.Left)))
                    transfer.Region = new Region(outline);
        };
        queue = ButtonFor("待传输", delegate { shell.ShowWindow("queue"); });
        queue.TextAlign = ContentAlignment.MiddleLeft;
        queue.FlatAppearance.BorderSize = 0;
        queue.BackColor = Tint;
        queue.Font = Style.Type(10, true);
        transfer.Controls.Add(queue, 0, 0);
        sync = ButtonFor("立即同步", async delegate { await shell.SyncPending(); });
        transfer.Controls.Add(sync, 1, 0);
        var detail = new Panel { Dock = DockStyle.Fill, Margin = new Padding(4, 0, 4, 0) };
        summary = Caption("", 8.5F, false, Ink);
        projectBar = new ProgressBar { Dock = DockStyle.Bottom, Height = 5, Maximum = 1000,
            AccessibleName = "项目总进度", Visible = false };
        detail.Controls.Add(summary);
        detail.Controls.Add(projectBar);
        transfer.Controls.Add(detail, 0, 1);
        transfer.SetColumnSpan(detail, 2);
        layout.Controls.Add(transfer, 0, 1);

        var heading = Row(0, 48);
        heading.Controls.Add(Caption("最近项目", 9, true, Muted), 0, 0);
        var all = ButtonFor("全部", delegate { shell.ShowWindow("projects"); });
        all.FlatAppearance.BorderSize = 0;
        all.ForeColor = Accent;
        heading.Controls.Add(all, 1, 0);
        layout.Controls.Add(heading, 0, 2);
        for (int i = 0; i < projects.Length; i++)
        {
            var b = new ProjectButton();
            b.Click += delegate(object sender, EventArgs args) {
                var id = ((Button)sender).Tag as string;
                if (!String.IsNullOrEmpty(id)) shell.OpenProject(id);
            };
            projects[i] = b;
            layout.Controls.Add(b, 0, 3 + i);
        }

        var policy = Row(16, 0, 57);
        scheduleDot = new Panel { Dock = DockStyle.Fill, Margin = Padding.Empty };
        scheduleDot.Paint += delegate(object sender, PaintEventArgs e) {
            float size = 8 * e.Graphics.DpiX / 96F;
            Color color = shell.Busy ? Color.SteelBlue : shell.AutoSyncEnabled ? Color.SeaGreen : Muted;
            using (var brush = new SolidBrush(color))
            {
                e.Graphics.SmoothingMode = SmoothingMode.AntiAlias;
                e.Graphics.FillEllipse(brush, 1, (scheduleDot.Height - size) / 2, size, size);
            }
        };
        policy.Controls.Add(scheduleDot, 0, 0);
        schedule = Caption("", 9, false, Ink);
        policy.Controls.Add(schedule, 1, 0);
        pause = ButtonFor("", delegate { shell.ToggleAuto(); });
        policy.Controls.Add(pause, 2, 0);
        layout.Controls.Add(policy, 0, 6);
        layout.Controls.Add(new Panel { Dock = DockStyle.Fill, Margin = Padding.Empty,
            BackColor = SystemInformation.HighContrast ? SystemColors.WindowText : Style.Line }, 0, 7);

        var footer = Row(100, 87, 0, 32);
        footer.Padding = new Padding(0, 5, 0, 0);
        footer.Controls.Add(ButtonFor("打开主窗口", delegate { shell.ShowWindow("overview"); }, true), 0, 0);
        check = ButtonFor("立即检查", async delegate { await shell.CheckNow(); });
        footer.Controls.Add(check, 1, 0);
        footer.Controls.Add(IconAction("power", "退出心狐同步", delegate { shell.Quit(); }), 3, 0);
        layout.Controls.Add(footer, 0, 8);
        Deactivate += delegate { Hide(); };
        KeyPreview = true;
        KeyDown += delegate(object sender, KeyEventArgs args) {
            if (args.KeyCode == Keys.Escape) { args.Handled = true; Hide(); }
        };
        ResumeLayout();
        UpdateValues();
    }

    static TableLayoutPanel Row(params int[] widths)
    {
        var row = new TableLayoutPanel { Dock = DockStyle.Fill, Margin = Padding.Empty,
            ColumnCount = widths.Length, RowCount = 1 };
        row.RowStyles.Add(new RowStyle(SizeType.Percent, 100));
        foreach (int width in widths)
            row.ColumnStyles.Add(width == 0 ? new ColumnStyle(SizeType.Percent, 100) : new ColumnStyle(SizeType.Absolute, width));
        return row;
    }

    static Label Caption(string text, float size, bool bold, Color color)
    {
        var label = Style.Text(text, size, bold, color);
        label.Dock = DockStyle.Fill;
        return label;
    }

    static Button ButtonFor(string text, EventHandler handler, bool primary = false)
    {
        var b = Style.Action(text, handler, primary);
        b.AutoSize = false;
        b.MinimumSize = Size.Empty;
        b.AutoEllipsis = true;
        b.Dock = DockStyle.Fill;
        b.Margin = new Padding(1, 4, 3, 4);
        b.Padding = Padding.Empty;
        b.UseMnemonic = false;
        if (SystemInformation.HighContrast)
        {
            b.BackColor = primary ? SystemColors.Highlight : SystemColors.Control;
            b.ForeColor = primary ? SystemColors.HighlightText : SystemColors.ControlText;
            b.FlatAppearance.MouseOverBackColor = b.BackColor;
        }
        return b;
    }

    Button IconAction(string glyph, string description, EventHandler action)
    {
        var b = new Button { Dock = DockStyle.Fill, Margin = new Padding(0, 4, 0, 4),
            BackColor = Paper, FlatStyle = FlatStyle.Flat, Cursor = Cursors.Hand };
        b.FlatAppearance.MouseOverBackColor = Tint;
        b.Click += action;
        tips.SetToolTip(b, description);
        b.AccessibleName = description;
        b.FlatAppearance.BorderSize = 0;
        b.Paint += delegate(object sender, PaintEventArgs e) {
            float size = 17 * e.Graphics.DpiX / 96F;
            var area = new RectangleF((b.Width - size) / 2, (b.Height - size) / 2, size, size);
            if (glyph == "settings") Style.Glyph(e.Graphics, glyph, area, Muted);
            else using (var pen = new Pen(Muted, size / 10F) { StartCap = LineCap.Round, EndCap = LineCap.Round })
            {
                e.Graphics.SmoothingMode = SmoothingMode.AntiAlias;
                e.Graphics.DrawArc(pen, area.X + size * .12F, area.Y + size * .17F, size * .76F, size * .72F, -56, 292);
                e.Graphics.DrawLine(pen, area.X + size / 2, area.Y + size * .07F, area.X + size / 2, area.Y + size * .52F);
            }
        };
        return b;
    }

    sealed class ProjectButton : Button
    {
        readonly Font detail = Style.Type(8);
        string title, nas, git;
        public bool HasGit { get { return !String.IsNullOrEmpty(git); } }

        public ProjectButton()
        {
            Font = Style.Type(9, true);
            Dock = DockStyle.Fill;
            Margin = Padding.Empty;
            FlatStyle = FlatStyle.Flat;
            FlatAppearance.BorderSize = 0;
            UseMnemonic = false;
            Cursor = Cursors.Hand;
        }

        public void Bind(Record project, string error)
        {
            Enabled = project != null;
            Tag = Enabled ? project.Field("unitId") : null;
            nas = Enabled ? ProjectDisplay.Nas(project) : "";
            git = Enabled ? ProjectDisplay.Git(project) : "";
            if (git == "—") git = "";
            title = Enabled ? project.Field("name") : String.IsNullOrEmpty(error) ? "暂无近期项目" : "项目状态暂不可用";
            Text = title + (HasGit ? " · Git " + git : "") + (Enabled ? " · " + nas : "");
            AccessibleName = Text;
        }

        protected override void OnPaint(PaintEventArgs e)
        {
            var g = e.Graphics;
            float scale = g.DpiX / 96F;
            bool hover = Enabled && ClientRectangle.Contains(PointToClient(Cursor.Position));
            g.Clear(hover ? Tint : Paper);
            var flags = TextFormatFlags.VerticalCenter | TextFormatFlags.SingleLine | TextFormatFlags.EndEllipsis | TextFormatFlags.NoPrefix;
            int inset = (int)(25 * scale);
            float icon = 16 * scale;
            if (Enabled)
                Style.Glyph(g, "projects", new RectangleF(2 * scale, (Height - icon) / 2, icon, icon), Accent);
            else inset = 0;
            int statusWidth = Enabled ? Math.Min(Width / 3, TextRenderer.MeasureText(nas, detail).Width + (int)(4 * scale)) : 0;
            int right = Width - statusWidth;
            var label = new Rectangle(inset, 0, Math.Max(0, right - inset), HasGit ? Height / 2 + 2 : Height);
            TextRenderer.DrawText(g, title, Enabled ? Font : detail, label, Enabled ? Ink : Muted, flags);
            if (HasGit)
                TextRenderer.DrawText(g, "Git " + git, detail, new Rectangle(inset, label.Bottom - 1, label.Width, Height - label.Bottom), Muted, flags);
            if (Enabled)
                TextRenderer.DrawText(g, nas, detail, new Rectangle(right, 0, statusWidth, Height), Muted, flags | TextFormatFlags.Right);
            if (Focused && ShowFocusCues)
                ControlPaint.DrawFocusRectangle(g, Rectangle.Inflate(ClientRectangle, -1, -1), Ink, Paper);
        }

        protected override void OnMouseEnter(EventArgs e) { base.OnMouseEnter(e); Invalidate(); }
        protected override void OnMouseLeave(EventArgs e) { base.OnMouseLeave(e); Invalidate(); }
        protected override void Dispose(bool disposing) { if (disposing) detail.Dispose(); base.Dispose(disposing); }
    }

    protected override void OnSizeChanged(EventArgs e)
    {
        base.OnSizeChanged(e);
        if (ClientSize.Width < 2 || ClientSize.Height < 2) return;
        using (var outline = Style.Rounded(ClientRectangle, Math.Max(4, Padding.Top)))
            Region = new Region(outline);
    }

    protected override void OnPaint(PaintEventArgs e)
    {
        base.OnPaint(e);
        var bounds = new Rectangle(0, 0, ClientSize.Width - 1, ClientSize.Height - 1);
        if (bounds.Width < 2 || bounds.Height < 2) return;
        e.Graphics.SmoothingMode = SmoothingMode.AntiAlias;
        using (var outline = Style.Rounded(bounds, Math.Max(4, Padding.Top)))
        using (var pen = new Pen(SystemInformation.HighContrast ? SystemColors.WindowText : Style.Line))
            e.Graphics.DrawPath(pen, outline);
    }

    protected override void OnFormClosing(FormClosingEventArgs e)
    {
        if (e.CloseReason == CloseReason.UserClosing) { e.Cancel = true; Hide(); }
        base.OnFormClosing(e);
    }

    protected override void Dispose(bool disposing)
    {
        if (disposing) { tips.Dispose(); if (portrait != null) portrait.Dispose(); }
        base.Dispose(disposing);
    }

    public void UpdateValues()
    {
        layout.SuspendLayout();
        float scale = layout.RowStyles[0].Height / 54F;
        for (int i = 0; i < projects.Length; i++)
        {
            var b = projects[i];
            var project = i < shell.Projects.Count ? shell.Projects[i] : null;
            b.Bind(project, shell.ProjectError);
            b.Visible = project != null || i == 0;
            layout.RowStyles[i + 3].Height = project == null && i != 0 ? 0 : (b.HasGit ? 44 : 30) * scale;
            tips.SetToolTip(b, project == null ? shell.ProjectError : b.Text);
        }
        connection.Text = "梦枢天罗 · " + shell.Connection;
        tips.SetToolTip(connection, connection.Text);
        queue.Text = "待传输     " + (shell.Queued < 0 ? "待读取" : shell.Queued.ToString()) + "   ›";
        queue.AccessibleName = "查看传输队列，" + queue.Text;
        sync.Text = shell.SyncButtonText;
        sync.Enabled = shell.CanSyncPending;
        string transferText = shell.SyncResult ?? "";
        projectBar.Visible = false;
        if (shell.Transfers != null && shell.Transfers.Count > 0)
        {
            var active = ProjectTransferProgress.Group(shell.Transfers)[0];
            string unit = active.Row.Field("unitId");
            var project = shell.Projects.FirstOrDefault(p => p.Field("unitId") == unit);
            transferText = (project == null ? unit : project.Field("name")) + " · " + active.Caption;
            active.Apply(projectBar);
        }
        summary.Text = transferText;
        tips.SetToolTip(summary, String.IsNullOrEmpty(shell.SyncErrorDetails) ? transferText : transferText + Environment.NewLine + shell.SyncErrorDetails);
        layout.RowStyles[1].Height = (transferText.Length == 0 ? 46 : 83) * scale;
        schedule.Text = shell.StatusText;
        scheduleDot.Invalidate();
        tips.SetToolTip(schedule, schedule.Text + (shell.LastChecked.HasValue ? " · 最近检查 " + shell.LastChecked.Value.ToString("HH:mm") : ""));
        pause.Text = shell.AutoSyncEnabled ? "暂停" : "启用";
        pause.AccessibleName = pause.Text + "自动检查";
        check.Enabled = !shell.Busy;
        ClientSize = new Size(ClientSize.Width, (int)Math.Ceiling(layout.RowStyles.Cast<RowStyle>().Sum(row => row.Height)) + Padding.Vertical);
        layout.ResumeLayout();
        if (Visible) PlaceAtAnchor();
    }

    void PlaceAtAnchor()
    {
        Rectangle bounds = Screen.FromPoint(anchor).WorkingArea;
        int x = Math.Min(Math.Max(bounds.Left, anchor.X - Width / 2), Math.Max(bounds.Left, bounds.Right - Width));
        int gap = Math.Max(4, Padding.Top / 2);
        int y = anchor.Y - Height - gap;
        if (y < bounds.Top) y = anchor.Y + gap;
        Location = new Point(x, Math.Min(Math.Max(bounds.Top, y), Math.Max(bounds.Top, bounds.Bottom - Height)));
    }

    public void ShowAt(Point point)
    {
        anchor = point;
        PlaceAtAnchor();
        UpdateValues();
        Show();
        PlaceAtAnchor();
        BringToFront();
        Activate();
    }
}
}
