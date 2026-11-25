using System;
using System.Drawing;
using System.Drawing.Drawing2D;
using System.Windows.Forms;

namespace XinhuSync
{
internal static class Style
{
    internal static readonly Color Canvas = Color.FromArgb(248, 246, 244), Rail = Color.FromArgb(242, 238, 235),
                                   Paper = Color.FromArgb(255, 254, 253), Ink = Color.FromArgb(41, 35, 38),
                                   Muted = Color.FromArgb(100, 89, 93), Plum = Color.FromArgb(138, 54, 84),
                                   Blush = Color.FromArgb(243, 225, 231), Line = Color.FromArgb(222, 214, 214);
    internal static Font Type(float size, bool bold = false)
    {
        return new Font("Microsoft YaHei UI", size, bold ? FontStyle.Bold : FontStyle.Regular);
    }
    internal static Label Text(string caption, float size, bool bold, Color color)
    {
        return new Label { AutoSize = false,
                           Text = caption,
                           Font = Type(size, bold),
                           ForeColor = color,
                           TextAlign = ContentAlignment.MiddleLeft,
                           BackColor = Color.Transparent,
                           AutoEllipsis = true,
                           Margin = Padding.Empty };
    }
    internal static Button Action(string caption, EventHandler click, bool primary = false)
    {
        var b = new Button { AutoSize = true,
                             Height = 32,
                             MinimumSize = new Size(0, 32),
                             FlatStyle = FlatStyle.Flat,
                             BackColor = primary ? Plum : Paper,
                             ForeColor = primary ? Color.White : Ink,
                             Cursor = Cursors.Hand,
                             Text = caption,
                             Padding = new Padding(9, 0, 9, 0),
                             Margin = new Padding(0, 2, 8, 2) };
        b.FlatAppearance.BorderSize = primary ? 0 : 1;
        b.FlatAppearance.BorderColor = Line;
        b.FlatAppearance.MouseOverBackColor = primary ? Color.FromArgb(113, 39, 68) : Blush;
        b.Click += click;
        return b;
    }
    // Symbol paths stay sharp on displays with a different scale factor.
    internal static void Glyph(Graphics g, string name, RectangleF bounds, Color color)
    {
        var state = g.Save();
        g.TranslateTransform(bounds.X, bounds.Y);
        g.ScaleTransform(bounds.Width / 24f, bounds.Height / 24f);
        g.SmoothingMode = SmoothingMode.AntiAlias;
        using (var p = new Pen(color, 1.7f) { StartCap = LineCap.Round, EndCap = LineCap.Round,
                                              LineJoin = LineJoin.Round })
        {
            switch (name)
            {
            case "overview":
                g.DrawRectangle(p, 3, 3, 7, 7);
                g.DrawRectangle(p, 14, 3, 7, 7);
                g.DrawRectangle(p, 3, 14, 7, 7);
                g.DrawRectangle(p, 14, 14, 7, 7);
                break;
            case "projects":
            case "libraries":
                g.DrawLines(p,
                            new[] { new Point(2, 7), new Point(2, 5), new Point(9, 5), new Point(11, 8),
                                    new Point(22, 8), new Point(21, 19), new Point(2, 19), new Point(2, 7) });
                break;
            case "queue":
                g.DrawLine(p, 8, 6, 21, 6);
                g.DrawLine(p, 8, 12, 19, 12);
                g.DrawLine(p, 8, 18, 16, 18);
                for (int i = 0; i < 3; i++)
                    g.DrawEllipse(p, 2, 5 + i * 6, 2, 2);
                break;
            case "cache":
                g.DrawLine(p, 12, 3, 12, 13);
                g.DrawLines(p, new[] { new Point(7, 9), new Point(12, 14), new Point(17, 9) });
                g.DrawLines(p,
                            new[] { new Point(3, 15), new Point(3, 20), new Point(21, 20), new Point(21, 15) });
                break;
            case "settings":
                for (int i = 0; i < 3; i++)
                {
                    int x = 4 + 8 * i, y = i == 1 ? 13 : 7;
                    g.DrawLine(p, x, 3, x, y - 2);
                    g.DrawEllipse(p, x - 2, y - 2, 4, 4);
                    g.DrawLine(p, x, y + 2, x, 21);
                }
                break;
            case "server":
                g.DrawRectangle(p, 2, 3, 20, 7);
                g.DrawRectangle(p, 2, 14, 20, 7);
                g.DrawLine(p, 6, 6.5f, 8, 6.5f);
                g.DrawLine(p, 6, 17.5f, 8, 17.5f);
                break;
            case "clock":
                g.DrawEllipse(p, 3, 3, 18, 18);
                g.DrawLines(p, new[] { new Point(12, 6), new Point(12, 12), new Point(16, 15) });
                break;
            }
        }
        g.Restore(state);
    }
    internal static Control Symbol(string name, Color color)
    {
        var c = new Panel { Size = new Size(36, 36), BackColor = Color.Transparent, TabStop = false };
        c.Paint += delegate(object sender, PaintEventArgs e)
        {
            float s = Math.Min(c.Width, c.Height) * .60f;
            Glyph(e.Graphics, name, new RectangleF((c.Width - s) / 2, (c.Height - s) / 2, s, s), color);
        };
        return c;
    }
}
internal sealed class NavItem : Button
{
    readonly string key;
    bool active;
    public bool Active
    {
        set {
            active = value;
            Invalidate();
        }
    }
    public NavItem(string name, string caption)
    {
        key = name;
        Text = caption;
        AccessibleName = caption;
        AccessibleRole = AccessibleRole.PushButton;
        FlatStyle = FlatStyle.Flat;
        FlatAppearance.BorderSize = 0;
        BackColor = Style.Rail;
        TextAlign = ContentAlignment.MiddleLeft;
        Cursor = Cursors.Hand;
        Dock = DockStyle.Fill;
        Margin = new Padding(10, 2, 10, 2);
        Font = Style.Type(10);
    }
    protected override void OnPaint(PaintEventArgs e)
    {
        bool hover = ClientRectangle.Contains(PointToClient(Cursor.Position));
        var color = active ? Style.Plum : Style.Muted;
        e.Graphics.Clear(active ? Style.Blush : hover ? Style.Canvas : Style.Rail);
        float unit = Height / 40f;
        Style.Glyph(e.Graphics, key, new RectangleF(14 * unit, 11 * unit, 18 * unit, 18 * unit), color);
        TextRenderer.DrawText(
            e.Graphics, Text, Font, new Rectangle((int)(43 * unit), 0, Width - (int)(44 * unit), Height),
            active ? Style.Plum : Style.Ink,
            TextFormatFlags.Left | TextFormatFlags.VerticalCenter | TextFormatFlags.EndEllipsis);
        if (Focused && ShowFocusCues)
            ControlPaint.DrawFocusRectangle(e.Graphics, Rectangle.Inflate(ClientRectangle, -3, -3));
    }
    protected override void OnMouseEnter(EventArgs e)
    {
        base.OnMouseEnter(e);
        Invalidate();
    }
    protected override void OnMouseLeave(EventArgs e)
    {
        base.OnMouseLeave(e);
        Invalidate();
    }
}

internal sealed class Surface : Panel
{
    public Surface()
    {
        Dock = DockStyle.Fill;
        BackColor = Style.Paper;
        SetStyle(ControlStyles.OptimizedDoubleBuffer | ControlStyles.ResizeRedraw, true);
    }
    protected override void OnPaint(PaintEventArgs e)
    {
        base.OnPaint(e);
        using (var p = new Pen(Style.Line)) e.Graphics.DrawRectangle(p, 0, 0, Width - 1, Height - 1);
    }
}
}
