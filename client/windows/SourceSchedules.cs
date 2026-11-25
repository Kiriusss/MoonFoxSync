using System;
using System.Collections.Generic;
using System.Drawing;
using System.Globalization;
using System.Linq;
using System.Threading.Tasks;
using System.Windows.Forms;

namespace XinhuSync
{
internal sealed partial class MainWindow
{
    async Task SourceSchedules()
    {
        var bindings = (await Bridge.Call(null, "bindings"))
                           .Rows()
                           .Where(b => b.Field("profile") == "file" || b.Field("profile") == "project")
                           .ToList();
        if (bindings.Count == 0)
        {
            MessageBox.Show(this, "本机尚未绑定文件或项目。", "按来源设置");
            return;
        }
        using (var dialog = new SourceScheduleForm(bindings)) dialog.ShowDialog(this);
    }
}

internal sealed class SourceScheduleForm : Form
{
    readonly ComboBox sources = List(), policy = List();
    readonly NumericUpDown minutes =
        new NumericUpDown { Minimum = 1, Maximum = 1440, Value = 1, Width = 105 };
    readonly Label message = Style.Text("", 9, false, Style.Muted);
    readonly string[] codes = { "network", "home", "manual" };
    readonly Button save;
    bool loading, saving;
    int revision;
    static ComboBox List()
    {
        return new ComboBox { DropDownStyle = ComboBoxStyle.DropDownList, Dock = DockStyle.Fill };
    }
    public SourceScheduleForm(List<Record> bindings)
    {
        Text = "按来源设置";
        Font = Style.Type(9);
        BackColor = Style.Paper;
        StartPosition = FormStartPosition.CenterParent;
        MinimumSize = new Size(600, 300);
        ClientSize = new Size(680, 270);
        Padding = new Padding(17);
        var form = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 2, RowCount = 5 };
        form.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, 128));
        form.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        for (int i = 0; i < 5; i++)
            form.RowStyles.Add(new RowStyle(SizeType.Percent, 20));
        string[] labels = { "本地文件或项目", "同步策略", "检查间隔（分钟）" };
        for (int i = 0; i < labels.Length; i++)
        {
            var label = Style.Text(labels[i], 10, true, Style.Ink);
            label.Dock = DockStyle.Fill;
            form.Controls.Add(label, 0, i);
        }
        sources.Anchor = policy.Anchor = AnchorStyles.Left | AnchorStyles.Right;
        minutes.Anchor = AnchorStyles.Left;
        form.Controls.Add(sources, 1, 0);
        form.Controls.Add(policy, 1, 1);
        form.Controls.Add(minutes, 1, 2);
        foreach (var b in bindings)
            sources.Items.Add(
                new Choice(b, (b.Field("profile") == "file" ? "文件" : "项目") + " · " + b.Field("path")));
        policy.Items.AddRange(new object[] { "跟随全局网络策略", "仅在家庭网络自动同步", "仅手动同步" });
        message.Dock = DockStyle.Fill;
        form.Controls.Add(message, 0, 3);
        form.SetColumnSpan(message, 2);
        var buttons = new FlowLayoutPanel { Dock = DockStyle.Fill, FlowDirection = FlowDirection.RightToLeft };
        save = Style.Action("保存此来源", async delegate { await Save(); }, true);
        buttons.Controls.Add(save);
        buttons.Controls.Add(Style.Action("关闭", delegate {
            if (!saving)
                Close();
        }));
        form.Controls.Add(buttons, 0, 4);
        form.SetColumnSpan(buttons, 2);
        Controls.Add(form);
        sources.SelectedIndexChanged += async delegate
        {
            await Read();
        };
        Shown += delegate
        {
            sources.SelectedIndex = 0;
        };
        FormClosing += delegate(object sender, FormClosingEventArgs e)
        {
            if (saving && e.CloseReason == CloseReason.UserClosing)
                e.Cancel = true;
        };
    }
    async Task Read()
    {
        var source = sources.SelectedItem as Choice;
        if (source == null)
            return;
        int seq = ++revision;
        loading = true;
        policy.Enabled = minutes.Enabled = save.Enabled = false;
        message.Text = "正在读取此来源的调度配置…";
        try
        {
            var row = await Bridge.Call(null, "schedule", "get", source.Row.Field("unitId"));
            if (IsDisposed || seq != revision)
                return;
            int match = Array.IndexOf(codes, row.Field("policy"));
            policy.SelectedIndex = match;
            int n;
            if (!Int32.TryParse(row.Field("intervalMinutes"), out n) || n < 1 || n > 1440)
                throw new InvalidOperationException("检查间隔无效。");
            minutes.Value = n;
            policy.Enabled = minutes.Enabled = save.Enabled = true;
            message.Text = source.Row.Field("path");
        }
        catch (Exception ex)
        {
            if (!IsDisposed && seq == revision)
                message.Text = ex.Message;
        }
        finally
        {
            if (seq == revision)
                loading = false;
        }
    }
    async Task Save()
    {
        var source = sources.SelectedItem as Choice;
        if (source == null || loading || saving || policy.SelectedIndex < 0)
            return;
        saving = true;
        sources.Enabled = policy.Enabled = minutes.Enabled = save.Enabled = false;
        try
        {
            var row = await Bridge.Call(null, "schedule", "set", source.Row.Field("unitId"), "--policy",
                                        codes[policy.SelectedIndex], "--interval",
                                        minutes.Value.ToString(CultureInfo.InvariantCulture));
            if (row.Field("unitId") != source.Row.Field("unitId"))
                throw new InvalidOperationException("调度回执与所选来源不一致，请重新读取。");
            message.Text = "已保存 · " + source.Row.Field("path");
        }
        catch (Exception ex)
        {
            message.Text = ex.Message;
        }
        finally
        {
            saving = false;
            sources.Enabled = policy.Enabled = minutes.Enabled = save.Enabled = true;
        }
    }
}
}
