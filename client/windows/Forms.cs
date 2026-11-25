using System;
using System.Drawing;
using System.IO;
using System.Text;
using System.Windows.Forms;

namespace XinhuSync
{
internal sealed class PairForm : Form
{
    private readonly TextBox server = new TextBox(), ca = new TextBox(), code = new TextBox();
    public string Address
    {
        get {
            return server.Text.Trim();
        }
    }
    public string CA
    {
        get {
            return ca.Text.Trim();
        }
    }
    public string Code
    {
        get {
            return code.Text.Trim();
        }
    }
    public PairForm()
    {
        Text = "配对梦枢天罗";
        StartPosition = FormStartPosition.CenterParent;
        AutoScaleMode = AutoScaleMode.Dpi;
        FormBorderStyle = FormBorderStyle.FixedDialog;
        MinimizeBox = MaximizeBox = false;
        Font = Style.Type(9);
        BackColor = Style.Paper;
        ForeColor = Style.Ink;
        ClientSize = new Size(490, 300);
        var layout = new TableLayoutPanel { Dock = DockStyle.Fill, Padding = new Padding(16), ColumnCount = 3,
                                            RowCount = 4 };
        layout.ColumnStyles.Add(new ColumnStyle(SizeType.AutoSize));
        layout.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        layout.ColumnStyles.Add(new ColumnStyle(SizeType.AutoSize));
        for (int i = 0; i < 4; i++)
            layout.RowStyles.Add(new RowStyle(SizeType.Percent, 25));
        layout.Controls.Add(new Label { Text = "服务地址", AutoSize = true, Anchor = AnchorStyles.Left }, 0, 0);
        layout.Controls.Add(new Label { Text = "CA 证书", AutoSize = true, Anchor = AnchorStyles.Left }, 0, 1);
        layout.Controls.Add(new Label { Text = "配对码", AutoSize = true, Anchor = AnchorStyles.Left }, 0, 2);
        server.Text = "https://";
        server.AccessibleName = "服务地址";
        ca.AccessibleName = "CA 证书";
        code.AccessibleName = "一次性配对码";
        code.UseSystemPasswordChar = true;
        var boxes = new[] { server, ca, code };
        for (int i = 0; i < 3; i++)
        {
            boxes[i].Dock = DockStyle.Fill;
            layout.Controls.Add(boxes[i], 1, i);
        }
        layout.SetColumnSpan(server, 2);
        layout.SetColumnSpan(code, 2);
        var choose = Style.Action("选择…", delegate {});
        choose.Click += delegate
        {
            using (var d = new OpenFileDialog {
                Filter = "PEM 证书|*.pem;*.crt;*.cer|所有文件|*.*"
            }) if (d.ShowDialog(this) == DialogResult.OK) ca.Text = d.FileName;
        };
        layout.Controls.Add(choose, 2, 1);
        var buttons = new FlowLayoutPanel { Dock = DockStyle.Fill, FlowDirection = FlowDirection.RightToLeft };
        var connect = Style.Action("配对", delegate {}, true);
        connect.Click += delegate
        {
            Uri u;
            if (!Uri.TryCreate(Address, UriKind.Absolute, out u) || u.Scheme != "https" || Code == "")
            {
                MessageBox.Show(this, "请输入 HTTPS 服务地址和一次性配对码。");
                return;
            }
            DialogResult = DialogResult.OK;
        };
        var cancel = Style.Action("取消", delegate {});
        cancel.DialogResult = DialogResult.Cancel;
        buttons.Controls.Add(connect);
        buttons.Controls.Add(cancel);
        layout.Controls.Add(buttons, 0, 3);
        layout.SetColumnSpan(buttons, 3);
        Controls.Add(layout);
        var title = Style.Text("连接梦枢天罗", 16, true, Style.Ink);
        title.Dock = DockStyle.Top;
        title.Height = 62;
        title.Padding = new Padding(16, 8, 0, 0);
        Controls.Add(title);
        AcceptButton = connect;
        CancelButton = cancel;
    }
}

internal static class ViewFile
{
    public static void Show(Form owner, string path, string name)
    {
        FileInfo info = new FileInfo(path);
        byte[] prefix = new byte[(int)Math.Min(info.Length, 65536L)];
        int used = 0;
        using (var f = info.OpenRead())
        {
            while (used < prefix.Length)
            {
                int n = f.Read(prefix, used, prefix.Length - used);
                if (n == 0)
                    break;
                used += n;
            }
        }
        string text = null;
        if (Array.IndexOf(prefix, (byte)0, 0, used) < 0)
        {
            try
            {
                var decoder = new UTF8Encoding(false, true).GetDecoder();
                char[] chars = new char[used];
                int count = decoder.GetChars(prefix, 0, used, chars, 0, used == info.Length);
                text = new string(chars, 0, count);
                foreach (char c in text)
                    if (Char.IsControl(c) && c != '\r' && c != '\n' && c != '\t')
                    {
                        text = null;
                        break;
                    }
            }
            catch (DecoderFallbackException)
            {
            }
        }
        using (var dialog = new Form { Text = "查看 · " + name, StartPosition = FormStartPosition.CenterParent,
                                       Size = new Size(760, 550), MinimumSize = new Size(435, 300) })
        {
            var bar =
                new FlowLayoutPanel { Dock = DockStyle.Bottom, AutoSize = true, Padding = new Padding(8) };
            var save = new Button { Text = "另存副本…", AutoSize = true };
            save.Click += delegate
            {
                using (var d = new SaveFileDialog { FileName = Path.GetFileName(name),
                                                    OverwritePrompt = true }) if (d.ShowDialog(dialog) ==
                                                                                  DialogResult.OK)
                {
                    try
                    {
                        string tmp = Path.Combine(Path.GetDirectoryName(d.FileName),
                                                  ".xinhu-" + Guid.NewGuid().ToString("N") + ".tmp");
                        try
                        {
                            File.Copy(path, tmp, false);
                            File.Move(tmp, d.FileName);
                        }
                        finally
                        {
                            if (File.Exists(tmp))
                                File.Delete(tmp);
                        }
                    }
                    catch (Exception ex)
                    {
                        MessageBox.Show(dialog, ex.Message, "另存失败");
                    }
                }
            };
            bar.Controls.Add(save);
            var caption = new Label { Dock = DockStyle.Top, AutoSize = true, Padding = new Padding(10),
                                      Text = String.Format("{0:N0} 字节{1}", info.Length,
                                                           text == null          ? " · 可另存后使用对应程序打开"
                                                           : info.Length > 65536 ? " · 预览前 64 KiB"
                                                                                 : " · UTF-8 文本") };
            if (text != null)
                dialog.Controls.Add(new TextBox { Multiline = true, WordWrap = false,
                                                  ScrollBars = ScrollBars.Both, ReadOnly = true,
                                                  Dock = DockStyle.Fill, Text = text,
                                                  Font = new Font("Consolas", 10) });
            dialog.Controls.Add(caption);
            dialog.Controls.Add(bar);
            dialog.ShowDialog(owner);
        }
    }
}
}
