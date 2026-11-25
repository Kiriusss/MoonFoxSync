using System;
using System.Collections.Generic;
using System.Drawing;
using System.Linq;
using System.Text;
using System.Threading.Tasks;
using System.Web.Script.Serialization;
using System.Windows.Forms;

namespace XinhuSync
{
internal sealed class MergeConflict
{
    public readonly Record Entry;
    public string Choice = "", Text;
    public string Path
    {
        get {
            return Entry.Field("path");
        }
    }
    public MergeConflict(Record entry)
    {
        Entry = entry;
    }
    public Dictionary<string, object> Resolution()
    {
        var r = new Dictionary<string, object> { { "path", Path }, { "choice", Choice } };
        switch (Choice)
        {
        case "main":
        case "branch":
            break;
        case "both":
            if (Entry.Field("bothPath") == "")
                throw new InvalidOperationException("此路径不能同时保留。");
            break;
        case "text":
            if (!String.Equals(Entry.Field("textAllowed"), "true", StringComparison.OrdinalIgnoreCase) ||
                Text == null)
                throw new InvalidOperationException("请先保存此路径的合并文本。");
            r.Add("text", Text);
            break;
        default:
            throw new InvalidOperationException("请处理冲突：" + Path);
        }
        return r;
    }
}

internal sealed class MergeWorkflow : Form
{
    readonly string unit, branch;
    readonly ListView conflicts =
        new ListView { Dock = DockStyle.Fill, View = View.Details, FullRowSelect = true, MultiSelect = false,
                       HideSelection = false };
    readonly ListBox automatic =
        new ListBox { Dock = DockStyle.Fill, HorizontalScrollbar = true, IntegralHeight = false };
    readonly TabControl lists = new TabControl { Dock = DockStyle.Fill },
                        versions = new TabControl { Dock = DockStyle.Fill };
    readonly Label message = Style.Text("", 9, false, Style.Muted),
                   member = Style.Text("", 11, true, Style.Ink),
                   destination = Style.Text("", 9, false, Style.Muted);
    readonly ComboBox decision = new ComboBox { DropDownStyle = ComboBoxStyle.DropDownList, Width = 155 };
    readonly TextBox versionName =
        new TextBox { Width = 190, Text = "合并分叉", Margin = new Padding(2, 8, 12, 0) };
    readonly Button edit, refresh, commit;
    readonly TextBox[] ids = new TextBox[3], bodies = new TextBox[3];
    readonly Label[] details = new Label[3];
    readonly string[] sideNames = { "主线", "分叉", "共同祖先" },
                      kindKeys = { "mainKind", "branchKind", "baseKind" };
    readonly List<MergeConflict> items = new List<MergeConflict>();
    Record plan;
    MergeConflict current;
    Record[] comparison;
    int comparisonTicket;
    bool changing, working, stale;
    string request, lastPayload, lastName;
    string automaticPath;
    public string Published { get; private set; }

    public MergeWorkflow(string project, string unitId, string snapshot)
    {
        unit = unitId;
        branch = snapshot;
        Text = "手动合并分叉 · " + project;
        Font = Style.Type(9);
        BackColor = Style.Canvas;
        StartPosition = FormStartPosition.CenterParent;
        AutoScaleDimensions = new SizeF(96, 96);
        AutoScaleMode = AutoScaleMode.Dpi;
        MinimumSize = new Size(900, 565);
        ClientSize = new Size(1050, 650);
        Padding = new Padding(16);
        var layout = Column();
        Controls.Add(layout);
        var h = Style.Text("将分叉合并到当前 NAS 主线", 16, true, Style.Ink);
        Row(layout, h, 41);
        var summary = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 3, RowCount = 2 };
        for (int i = 0; i < 3; i++)
        {
            summary.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100f / 3));
            var title = Style.Text(sideNames[i], 9, true, Style.Muted);
            title.Dock = DockStyle.Fill;
            summary.Controls.Add(title, i, 0);
            ids[i] = new TextBox { Dock = DockStyle.Fill, ReadOnly = true, BackColor = Style.Paper,
                                   Margin = new Padding(0, 1, 10, 0) };
            summary.Controls.Add(ids[i], i, 1);
        }
        Row(layout, summary, 60);
        var split = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 2, RowCount = 1 };
        split.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 37));
        split.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 63));
        var conflictTab = new TabPage("需处理");
        conflicts.Columns.Add("路径");
        conflicts.Columns.Add("选择");
        conflicts.ShowItemToolTips = true;
        conflicts.SizeChanged += delegate
        {
            int w = Math.Max(120, conflicts.ClientSize.Width - SystemInformation.VerticalScrollBarWidth - 2);
            conflicts.Columns[0].Width = w * 2 / 3;
            conflicts.Columns[1].Width = w - w * 2 / 3;
        };
        conflictTab.Controls.Add(conflicts);
        lists.TabPages.Add(conflictTab);
        var autoTab = new TabPage("自动合并");
        autoTab.Controls.Add(automatic);
        lists.TabPages.Add(autoTab);
        split.Controls.Add(lists, 0, 0);
        var inspector = Column();
        inspector.Padding = new Padding(8, 0, 0, 0);
        Row(inspector, member, 36);
        var choose = new FlowLayoutPanel { Dock = DockStyle.Fill, WrapContents = false };
        choose.Controls.Add(decision);
        edit = Style.Action("编辑合并文本…", delegate { Edit(); });
        choose.Controls.Add(edit);
        Row(inspector, choose, 39);
        Row(inspector, destination, 30);
        for (int i = 0; i < 3; i++)
        {
            var tab = new TabPage(sideNames[i]);
            details[i] = Style.Text("", 9, false, Style.Muted);
            details[i].Height = 28;
            details[i].Dock = DockStyle.Top;
            bodies[i] = new TextBox { ReadOnly = true,
                                      Multiline = true,
                                      ScrollBars = ScrollBars.Both,
                                      WordWrap = false,
                                      Dock = DockStyle.Fill,
                                      BackColor = Style.Paper,
                                      Font = new Font("Consolas", 10),
                                      BorderStyle = BorderStyle.None };
            tab.Controls.Add(bodies[i]);
            tab.Controls.Add(details[i]);
            versions.TabPages.Add(tab);
        }
        Row(inspector, versions, -1);
        split.Controls.Add(inspector, 1, 0);
        Row(layout, split, -1);
        Row(layout, message, 35);
        var actions = new FlowLayoutPanel { Dock = DockStyle.Fill, WrapContents = false };
        var name = Style.Text("新版本名", 9, true, Style.Ink);
        name.AutoSize = true;
        name.Margin = new Padding(0, 9, 5, 0);
        actions.Controls.Add(name);
        actions.Controls.Add(versionName);
        refresh = Style.Action("重新预览", async delegate { await LoadPlan(true); });
        actions.Controls.Add(refresh);
        commit = Style.Action("确认并发布…", async delegate { await Commit(); }, true);
        actions.Controls.Add(commit);
        actions.Controls.Add(Style.Action("关闭", delegate {
            if (!working)
                Close();
        }));
        Row(layout, actions, 43);
        automatic.SelectedIndexChanged += async delegate
        {
            if (!changing)
                await SelectConflict();
        };
        conflicts.SelectedIndexChanged += async delegate
        {
            if (!changing)
                await SelectConflict();
        };
        lists.SelectedIndexChanged += async delegate
        {
            if (!changing)
                await SelectConflict();
        };
        decision.SelectedIndexChanged += delegate
        {
            if (changing || working || stale || current == null || decision.SelectedIndex < 0)
                return;
            string next = ((ResolutionOption)decision.SelectedItem).Value;
            if (next == "text" && current.Text == null)
            {
                Edit();
                return;
            }
            current.Choice = next;
            UpdateDecisionUI();
        };
        FormClosing += delegate(object sender, FormClosingEventArgs e)
        {
            if (working && e.CloseReason == CloseReason.UserClosing)
                e.Cancel = true;
        };
        Shown += async delegate
        {
            await LoadPlan(false);
        };
        SetControls();
    }
    static TableLayoutPanel Column()
    {
        var c = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 1, RowCount = 0,
                                       Margin = Padding.Empty };
        c.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        return c;
    }
    static void Row(TableLayoutPanel t, Control c, int h)
    {
        int i = t.RowCount++;
        t.RowStyles.Add(h < 0 ? new RowStyle(SizeType.Percent, 100) : new RowStyle(SizeType.Absolute, h));
        c.Dock = DockStyle.Fill;
        t.Controls.Add(c, 0, i);
    }
    sealed class ResolutionOption
    {
        public readonly string Value, Title;
        public ResolutionOption(string value, string title)
        {
            Value = value;
            Title = title;
        }
        public override string ToString()
        {
            return Title;
        }
    }
    static string Kind(string v)
    {
        switch (v)
        {
        case "file":
            return "文件";
        case "text":
            return "文本";
        case "binary":
            return "二进制文件";
        case "directory":
            return "目录";
        case "symlink":
            return "符号链接";
        case "hidden":
            return "原时间点已移除";
        case "missing":
            return "不存在";
        default:
            return v;
        }
    }
    static string LabelFor(string v)
    {
        switch (v)
        {
        case "main":
            return "保留主线";
        case "branch":
            return "采用分叉";
        case "both":
            return "同时保留";
        case "text":
            return "手动文本";
        default:
            return "待选择";
        }
    }
    void SetControls()
    {
        bool allowed = plan != null && !working && !stale && !BooleanValue(plan, "alreadyMerged");
        decision.Enabled = allowed && current != null;
        edit.Enabled = allowed && current != null && Editable();
        refresh.Enabled = !working;
        commit.Enabled = allowed && items.All(i => i.Choice != "");
        versionName.Enabled = allowed;
        lists.Enabled = !working;
    }
    static bool BooleanValue(Record r, string key)
    {
        return String.Equals(r.Field(key), "true", StringComparison.OrdinalIgnoreCase);
    }
    async Task LoadPlan(bool confirm)
    {
        if (working)
            return;
        if (confirm && items.Any(x => x.Choice != "") &&
            MessageBox.Show(this, "重新读取 NAS 的主线与分叉后，需要重新核对冲突选择。", "重新预览",
                            MessageBoxButtons.OKCancel) != DialogResult.OK)
            return;
        working = true;
        stale = true;
        comparisonTicket++;
        SetControls();
        message.Text = "正在读取 NAS 三方合并计划…";
        try
        {
            var result = await Bridge.Call(null, "project", "merge-plan", unit, "--branch", branch);
            if (result.Field("unitId") != unit || result.Field("branchSnapshotId") != branch ||
                result.Field("mainHead") == "")
                throw new InvalidOperationException("返回的合并目标与所选版本不一致。");
            var values = result["conflicts"] == null ? new List<Record>() : result.Rows("conflicts");
            changing = true;
            try
            {
                current = null;
                comparison = null;
                items.Clear();
                conflicts.Items.Clear();
                automatic.Items.Clear();
                foreach (var r in values)
                {
                    var c = new MergeConflict(r);
                    items.Add(c);
                    conflicts.Items.Add(
                        new ListViewItem(new[] { c.Path, "待选择" }) { Tag = c, ToolTipText = c.Path });
                }
                foreach (var entry in (result["autoPaths"] as System.Collections.IEnumerable) ?? new object[0])
                    automatic.Items.Add(Convert.ToString(entry));
                plan = result;
                request = Guid.NewGuid().ToString("D");
                lastName = null;
                lastPayload = null;
                ids[0].Text = plan.Field("mainHead");
                ids[1].Text = branch;
                ids[2].Text = plan.Field("baseSnapshotId");
                if (BooleanValue(plan, "baseHidden"))
                    ids[2].Text = "原时间点已移除";
                else if (ids[2].Text == "")
                    ids[2].Text = "空基线";
                lists.TabPages[0].Text = "需处理 · " + items.Count;
                lists.TabPages[1].Text = "自动合并 · " + automatic.Items.Count;
                if (items.Count > 0)
                {
                    lists.SelectedIndex = 0;
                    conflicts.Items[0].Selected = true;
                }
                else
                    lists.SelectedIndex = 1;
                stale = false;
            }
            finally
            {
                changing = false;
            }
            message.Text = BooleanValue(plan, "alreadyMerged") ? "所选分叉已合并。"
                           : BooleanValue(plan, "baseAmbiguous")
                               ? "存在多个共同祖先，显示其中一个用于对照，请逐项处理所有差异。"
                               : "核对自动变更与逐项选择后，可发布一个新的 NAS 快照。";
        }
        catch (Exception ex)
        {
            message.Text = ex.Message;
        }
        finally
        {
            working = false;
            SetControls();
        }
        await SelectConflict();
    }
    async Task SelectConflict()
    {
        int ticket = ++comparisonTicket;
        current = lists.SelectedIndex == 0 && conflicts.SelectedItems.Count > 0
                      ? (MergeConflict)conflicts.SelectedItems[0].Tag
                      : null;
        automaticPath = lists.SelectedIndex == 1 ? automatic.SelectedItem as string : null;
        comparison = null;
        UpdateDecisionUI();
        for (int i = 0; i < 3; i++)
        {
            details[i].Text = current == null ? "" : Kind(current.Entry.Field(kindKeys[i]));
            bodies[i].Text = "";
        }
        if ((current == null && automaticPath == null) || plan == null || stale)
            return;
        var selected = current;
        string memberPath = selected == null ? automaticPath : selected.Path;
        var paths = new[] { plan.Field("mainHead"), branch, plan.Field("baseSnapshotId") };
        await Task.Delay(120);
        if (IsDisposed || ticket != comparisonTicket)
            return;
        var readers = Enumerable.Range(0, 3).Select(
            async i =>
            {
                if (i == 2 && BooleanValue(plan, "baseHidden"))
                    return new Record(new Dictionary<string, object> { { "kind", "hidden" } });
                if ((selected != null && selected.Entry.Field(kindKeys[i]) == "missing") || paths[i] == "")
                    return new Record(new Dictionary<string, object> { { "kind", "missing" } });
                try
                {
                    return await Bridge.Call(null, "project", "merge-read", unit, "--snapshot", paths[i],
                                             "--path", memberPath);
                }
                catch (Exception ex)
                {
                    return new Record(
                        new Dictionary<string, object> { { "kind", "error" }, { "message", ex.Message } });
                }
            });
        var result = await Task.WhenAll(readers);
        if (IsDisposed || ticket != comparisonTicket)
            return;
        comparison = result;
        for (int i = 0; i < 3; i++)
        {
            var v = result[i];
            string k = v.Field("kind");
            string caption = Kind(k);
            if (k == "error")
                caption = "读取失败";
            string size = MainWindow.FormatBytes(v.Field("size"));
            details[i].Text = caption + (size == "" ? "" : " · " + size) +
                              (BooleanValue(v, "truncated") ? " · 部分预览" : "");
            bodies[i].Text = k == "text"        ? WindowsLines(v.Field("text"))
                             : k == "symlink"   ? v.Field("target")
                             : k == "error"     ? v.Field("message")
                             : k == "hidden"    ? "原时间点已移除，祖先内容不可读取。"
                             : k == "missing"   ? "此版本没有该路径。"
                             : k == "directory" ? "此路径为目录。"
                                                : "此文件为二进制内容。";
        }
        UpdateDecisionUI();
    }
    bool Editable()
    {
        if (current == null || !BooleanValue(current.Entry, "textAllowed") || comparison == null)
            return false;
        return comparison.Any(r => r.Field("kind") == "text") &&
               comparison.All(r => r.Field("kind") == "missing" || r.Field("kind") == "hidden" ||
                                   r.Field("kind") == "text" && !BooleanValue(r, "truncated"));
    }
    static string WindowsLines(string text)
    {
        return text.Replace("\r\n", "\n").Replace("\n", "\r\n");
    }
    void UpdateDecisionUI()
    {
        changing = true;
        try
        {
            decision.Items.Clear();
            if (current == null)
            {
                member.Text = automaticPath ?? (items.Count == 0 ? "无待处理冲突" : "选择路径以对照三个版本");
                destination.Text = "";
            }
            else
            {
                member.Text = current.Path;
                string both = current.Entry.Field("bothPath");
                destination.Text = both == "" ? "" : "同时保留时，分叉另存为 " + both;
                var choices = new List<string> { "", "main", "branch" };
                if (both != "")
                    choices.Add("both");
                if (Editable() || current.Choice == "text")
                    choices.Add("text");
                foreach (var value in choices)
                    decision.Items.Add(new ResolutionOption(value, LabelFor(value)));
                decision.SelectedIndex = choices.IndexOf(current.Choice);
                if (conflicts.SelectedItems.Count > 0)
                    conflicts.SelectedItems[0].SubItems[1].Text = LabelFor(current.Choice);
            }
        }
        finally
        {
            changing = false;
        }
        SetControls();
    }
    static string EditedText(string raw, string seed)
    {
        return raw == WindowsLines(seed) ? seed : seed.Contains("\r\n") ? raw : raw.Replace("\r\n", "\n");
    }
    void Edit()
    {
        if (!Editable() || current == null)
        {
            UpdateDecisionUI();
            return;
        }
        var seed = current.Text ?? comparison.First(r => r.Field("kind") == "text").Field("text");
        using (var form =
                   new Form { Text = "编辑合并文本 · " + current.Path,
                              StartPosition = FormStartPosition.CenterParent, ClientSize = new Size(760, 500),
                              Padding = new Padding(10), Font = Style.Type(9) })
        {
            var editor = new TextBox { Multiline = true,
                                       AcceptsReturn = true,
                                       AcceptsTab = true,
                                       WordWrap = false,
                                       ScrollBars = ScrollBars.Both,
                                       MaxLength = Int32.MaxValue,
                                       Font = new Font("Consolas", 10),
                                       Dock = DockStyle.Fill,
                                       Text = WindowsLines(seed) };
            var buttons = new FlowLayoutPanel { Dock = DockStyle.Bottom, Height = 42,
                                                FlowDirection = FlowDirection.RightToLeft };
            buttons.Controls.Add(Style.Action("采用此文本", delegate {
                string value = EditedText(editor.Text, seed);
                if (Encoding.UTF8.GetByteCount(value) > (128 << 10) || value.IndexOf('\0') >= 0)
                {
                    MessageBox.Show(form, "合并文本须为不含空字符的 UTF-8 文本，最多 128 KiB。");
                    return;
                }
                form.DialogResult = DialogResult.OK;
            }, true));
            buttons.Controls.Add(Style.Action("取消", delegate { form.DialogResult = DialogResult.Cancel; }));
            form.Controls.Add(editor);
            form.Controls.Add(buttons);
            if (form.ShowDialog(this) == DialogResult.OK)
            {
                string raw = editor.Text;
                current.Text = EditedText(raw, seed);
                current.Choice = "text";
            }
        }
        UpdateDecisionUI();
    }
    async Task Commit()
    {
        if (plan == null || stale || working || BooleanValue(plan, "alreadyMerged") ||
            items.Any(i => i.Choice == ""))
            return;
        string name = versionName.Text.Trim();
        if (name.Length == 0)
        {
            message.Text = "请输入新版本名。";
            return;
        }
        string body = new JavaScriptSerializer { MaxJsonLength = Int32.MaxValue }.Serialize(
            items.Select(i => i.Resolution()).ToArray());
        if (MessageBox.Show(this,
                            "新版本：" + name + "\n目标主线：" + plan.Field("mainHead") + "\n来源分叉：" +
                                branch + "\n自动合并 " + automatic.Items.Count + " 项，已处理冲突 " +
                                items.Count + " 项。\n\n现在发布到 NAS 主线？",
                            "确认发布合并快照", MessageBoxButtons.OKCancel,
                            MessageBoxIcon.Question) != DialogResult.OK)
            return;
        if (lastPayload != null && (lastPayload != body || lastName != name))
            request = Guid.NewGuid().ToString("D");
        lastPayload = body;
        lastName = name;
        working = true;
        SetControls();
        message.Text = "正在发布合并快照…";
        try
        {
            var receipt =
                await Bridge.Call(body, "project", "merge-commit", unit, "--main", plan.Field("mainHead"),
                                  "--branch", branch, "--request", request, "--name", name);
            Published = receipt.Field("snapshotId");
            if (Published == "")
                throw new InvalidOperationException("缺少合并版本回执，请用相同选择重试。");
            working = false;
            DialogResult = DialogResult.OK;
        }
        catch (Exception ex)
        {
            var msg = ex.Message;
            stale = msg.Contains("HTTP 409") || msg.Contains("HEAD_MOVED") || msg.Contains("STALE") ||
                    msg.Contains("主线已变化");
            message.Text = stale ? "NAS 状态已变化，请重新预览并核对新主线。" : msg;
            MessageBox.Show(this, message.Text, "合并尚未确认", MessageBoxButtons.OK,
                            MessageBoxIcon.Information);
        }
        finally
        {
            working = false;
            if (!IsDisposed)
                SetControls();
        }
    }
}
}
