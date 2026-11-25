using System;
using System.Drawing;
using System.Drawing.Drawing2D;
using System.Windows.Forms;

namespace XinhuSync
{
internal static class Style
{
    // Matches the light appearance in the macOS DeskColor palette.
    internal static readonly Color Canvas = Color.FromArgb(250, 249, 248), Rail = Color.FromArgb(245, 242, 242),
                                   Paper = Color.White, Ink = Color.FromArgb(48, 42, 48),
                                   Muted = Color.FromArgb(112, 103, 109), Plum = Color.FromArgb(152, 62, 87),
                                   Blush = Color.FromArgb(239, 223, 227), Line = Color.FromArgb(231, 226, 226);

    internal static GraphicsPath Rounded(RectangleF rectangle, float radius)
    {
        var path = new GraphicsPath();
        float d = Math.Min(2 * radius, Math.Min(rectangle.Width, rectangle.Height));
        path.AddArc(rectangle.Left, rectangle.Top, d, d, 180, 90);
        path.AddArc(rectangle.Right - d, rectangle.Top, d, d, 270, 90);
        path.AddArc(rectangle.Right - d, rectangle.Bottom - d, d, d, 0, 90);
        path.AddArc(rectangle.Left, rectangle.Bottom - d, d, d, 90, 90);
        path.CloseFigure();
        return path;
    }

    internal static Color Behind(Control control)
    {
        for (var parent = control.Parent; parent != null; parent = parent.Parent)
            if (parent.BackColor.A == 255)
                return parent.BackColor;
        return Canvas;
    }
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
        var b = new DeskButton { AutoSize = true,
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
internal sealed class DeskButton : Button
{
    bool hover, pressed;
    protected override void OnMouseEnter(EventArgs e) { base.OnMouseEnter(e); hover = true; Invalidate(); }
    protected override void OnMouseLeave(EventArgs e) { base.OnMouseLeave(e); hover = false; Invalidate(); }
    protected override void OnMouseDown(MouseEventArgs e) { base.OnMouseDown(e); pressed = true; Invalidate(); }
    protected override void OnMouseUp(MouseEventArgs e) { base.OnMouseUp(e); pressed = false; Invalidate(); }

    protected override void OnPaint(PaintEventArgs e)
    {
        if (SystemInformation.HighContrast || Width <= 2 || Height <= 2) { base.OnPaint(e); return; }
        var g = e.Graphics;
        float scale = g.DpiX / 96f;
        g.Clear(Style.Behind(this));
        g.SmoothingMode = SmoothingMode.AntiAlias;
        var surface = !Enabled ? Style.Rail : pressed && hover ? ControlPaint.Dark(BackColor, .12f)
                               : hover ? FlatAppearance.MouseOverBackColor : BackColor;
        if (surface.IsEmpty) surface = BackColor;
        using (var outline = Style.Rounded(new RectangleF(.5f, .5f, Width - 1, Height - 1), 7 * scale))
        {
            using (var brush = new SolidBrush(surface)) g.FillPath(brush, outline);
            bool focused = Focused && ShowFocusCues;
            if (FlatAppearance.BorderSize > 0 || focused)
                using (var pen = new Pen(focused ? Style.Plum : FlatAppearance.BorderColor, focused ? 2 : 1))
                    g.DrawPath(pen, outline);
        }
        var flags = TextFormatFlags.VerticalCenter | TextFormatFlags.EndEllipsis | TextFormatFlags.SingleLine;
        flags |= TextAlign == ContentAlignment.MiddleLeft ? TextFormatFlags.Left : TextFormatFlags.HorizontalCenter;
        flags |= UseMnemonic ? (ShowKeyboardCues ? 0 : TextFormatFlags.HidePrefix) : TextFormatFlags.NoPrefix;
        TextRenderer.DrawText(g, Text, Font, Rectangle.Inflate(ClientRectangle, -(int)(6 * scale), 0),
                              Enabled ? ForeColor : Style.Muted, flags);
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
        if (Width <= 2 || Height <= 2) return;
        var g = e.Graphics;
        g.Clear(Style.Behind(this));
        g.SmoothingMode = SmoothingMode.AntiAlias;
        using (var path = Style.Rounded(new RectangleF(.5f, .5f, Width - 1, Height - 1), 13 * g.DpiX / 96f))
        using (var brush = new SolidBrush(BackColor))
        using (var pen = new Pen(Style.Line))
        {
            g.FillPath(brush, path);
            g.DrawPath(pen, path);
        }
    }
}
}
