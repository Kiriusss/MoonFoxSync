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
    Task ProjectTimeline()
    {
        var project = SelectedProject();
        return OpenTimeline(project.Field("unitId"), project.Field("name"), "");
    }
    Task LibraryTimeline()
    {
        if (!Managed)
            throw new InvalidOperationException("请选择有历史版本的托管资料库。");
        return OpenTimeline(UnitId, Unit().Field("name"), SnapshotId);
    }
    async Task OpenTimeline(string unitId, string title, string selected)
    {
        using (var form = new TimelineWindow(unitId, title, selected))
        {
            UseWaitCursor = false;
            form.ShowDialog(this);
            if (!form.Changed)
                return;
        }
        if (page == "libraries")
            await RefreshUnits();
        await shell.RefreshProjects(false);
        if (page == "projects")
            RenderProjects();
        status.Text = "已刷新 NAS 时间线";
    }

    sealed class TimelineWindow : Form
    {
        readonly string unit, title;
        readonly ListView grid = Grid("版本名称", "类型", "保存时间", "版本 ID");
        readonly CheckBox onlyForks = Check("只看分叉");
        readonly TextBox parents = new TextBox { Dock = DockStyle.Fill,
                                                 ReadOnly = true,
                                                 BorderStyle = BorderStyle.FixedSingle,
                                                 BackColor = Style.Paper,
                                                 Multiline = true,
                                                 ScrollBars = ScrollBars.Vertical,
                                                 AccessibleName = "所选时间点的父版本" };
        readonly Label summary = Style.Text("正在读取版本…", 10, true, Style.Muted);
        readonly Label note = Style.Text("", 9, false, Style.Muted);
        readonly Button refresh, policy, copy, restore, delete, merge;
        List<Record> rows = new List<Record>();
        Record access;
        string head = "", preferred, restoreKey, restoreRequest;
        bool busy;
        public bool Changed { get; private set; }
        bool Writable
        {
            get {
                return access != null && access.Field("role") == "sync";
            }
        }
        Record Selection
        {
            get {
                return grid.SelectedItems.Count == 0 ? null : (Record)grid.SelectedItems[0].Tag;
            }
        }
        public TimelineWindow(string id, string name, string selection)
        {
            unit = id;
            title = name;
            preferred = selection;
            Text = "版本时间线 · " + title;
            Font = Style.Type(9);
            BackColor = Style.Canvas;
            StartPosition = FormStartPosition.CenterParent;
            AutoScaleMode = AutoScaleMode.Dpi;
            MinimumSize = new Size(750, 475);
            ClientSize = new Size(880, 530);
            Padding = new Padding(16);
            var body = Column();
            Controls.Add(body);
            Row(body, Occupy(Style.Text(title + " · 版本时间线", 16, true, Style.Ink)), 38);
            Row(body, Occupy(summary), 32);
            var top = Bar();
            refresh = Command("刷新", async delegate {
                await Reload();
                note.Text = "版本时间线已刷新";
            });
            policy = Command("历史保存策略…", EditPolicy);
            top.Controls.Add(refresh);
            top.Controls.Add(policy);
            top.Controls.Add(onlyForks);
            Row(body, top, 0);
            Row(body, GridCard(grid, note), -1);
            Row(body, parents, 50);
            var tools = Bar();
            restore = Command("恢复为当前版本…", Rollback, true);
            delete = Command("删除并重建…", Remove);
            copy = Command("恢复到新目录…", Download);
            merge = Command("手动合并分叉…", Merge);
            foreach (var button in new[] { restore, delete, copy, merge })
                tools.Controls.Add(button);
            tools.Controls.Add(Style.Action("关闭", delegate {
                if (!busy)
                    Close();
            }));
            Row(body, tools, 0);
            grid.Resize += delegate
            {
                int width = Math.Max(250, grid.ClientSize.Width - SystemInformation.VerticalScrollBarWidth - 6);
                int[] weights = { 31, 13, 22, 34 };
                for (int i = 0; i < weights.Length; i++)
                    grid.Columns[i].Width = width * weights[i] / 100;
            };
            onlyForks.CheckedChanged += delegate
            {
                Render();
            };
            grid.SelectedIndexChanged += delegate
            {
                SelectionDetails();
                Buttons();
            };
            FormClosing += delegate(object sender, FormClosingEventArgs e)
            {
                if (busy && e.CloseReason == CloseReason.UserClosing)
                    e.Cancel = true;
            };
            Shown += async delegate
            {
                await Execute(Reload);
            };
            Buttons();
        }
        Button Command(string caption, Func<Task> work, bool primary = false)
        {
            return Style.Action(caption, async delegate { await Execute(work); }, primary);
        }
        async Task Execute(Func<Task> work)
        {
            if (busy)
                return;
            busy = true;
            Buttons();
            try
            {
                Exception failure = null;
                try
                {
                    await work();
                }
                catch (Exception ex)
                {
                    failure = ex;
                }
                if (failure == null)
                    return;
                if (failure.Message.Contains("HTTP 409"))
                {
                    string detail = failure.Message;
                    try
                    {
                        await Reload();
                    }
                    catch (Exception refreshError)
                    {
                        detail += "\n刷新失败：" + refreshError.Message;
                    }
                    note.Text = "操作遇到冲突，请重新核对主线与时间点。";
                    MessageBox.Show(this, note.Text + "\n\n" + detail, "时间线状态已变化或版本受保护",
                                    MessageBoxButtons.OK, MessageBoxIcon.Information);
                }
                else
                {
                    note.Text = failure.Message;
                    MessageBox.Show(this, failure.Message, "版本时间线", MessageBoxButtons.OK,
                                    MessageBoxIcon.Information);
                }
            }
            finally
            {
                busy = false;
                Buttons();
            }
        }
        void Buttons()
        {
            bool ready = !busy && access != null;
            var pick = Selection;
            bool chosen = ready && pick != null;
            bool previous = chosen && head != "" && pick.Field("snapshotId") != head;
            refresh.Enabled = !busy;
            onlyForks.Enabled = grid.Enabled = policy.Enabled = ready;
            copy.Enabled = chosen;
            restore.Visible = delete.Visible = merge.Visible = Writable;
            restore.Enabled = previous && Writable;
            delete.Enabled = chosen && head != "" && Writable;
            merge.Enabled = chosen && Writable && pick.Field("outcome") == "fork";
        }
        async Task Reload()
        {
            Record previous = Selection;
            if (previous != null && String.IsNullOrEmpty(preferred))
                preferred = previous.Field("snapshotId");
            access = null;
            Buttons();
            var grants = (await Bridge.Call(null, "units")).Rows();
            var grant = grants.FirstOrDefault(r => r.Field("unitId") == unit && r.Field("kind") == "managed");
            if (grant == null)
                throw new InvalidOperationException("此设备当前没有该托管资料库的读取权限。");
            var history = (await Bridge.Call(null, "history", unit)).Rows();
            access = grant;
            head = grant.Field("head");
            rows = history;
            Render();
            summary.Text = "当前主线  " + (head == "" ? "暂无已发布版本" : head) + "     ·     " +
                           (Writable ? "可同步" : "只读");
        }
        void Render()
        {
            grid.BeginUpdate();
            try
            {
                string select = preferred ?? "";
                var current = Selection;
                if (select == "" && current != null)
                    select = current.Field("snapshotId");
                grid.Items.Clear();
                ListViewItem fallback = null;
                foreach (var r in rows)
                {
                    bool fork = r.Field("outcome") == "fork";
                    if (onlyForks.Checked && !fork)
                        continue;
                    string id = r.Field("snapshotId");
                    string when = r.Field("committedAt");
                    Add(grid, r, JobTitle(r),
                        id == head ? "当前主线"
                        : fork     ? "分叉"
                                   : "主线历史",
                        LocalTime(when == "" ? r.Field("capturedAt") : when), id);
                    var item = grid.Items[grid.Items.Count - 1];
                    if (fallback == null || id == head)
                        fallback = item;
                    if (id == select)
                    {
                        item.Selected = true;
                        item.Focused = true;
                        item.EnsureVisible();
                    }
                }
                if (grid.SelectedItems.Count == 0 && fallback != null)
                {
                    fallback.Selected = true;
                    fallback.Focused = true;
                }
                preferred = null;
                note.Text =
                    grid.Items.Count == 0 ? "没有符合条件的时间点" : "共 " + grid.Items.Count + " 个时间点";
            }
            finally
            {
                grid.EndUpdate();
            }
            Buttons();
        }
        void SelectionDetails()
        {
            var row = Selection;
            if (row == null)
            {
                parents.Text = "";
                return;
            }
            bool rebuilt =
                String.Equals(row.Field("timelineRebuilt"), "true", StringComparison.OrdinalIgnoreCase);
            var visible = rebuilt ? Strings(row, "timelineParentSnapshotIds").ToArray()
                                  : new[] { row.Field("parentSnapshotId"), row.Field("mergeParentSnapshotId") }
                                        .Where(id => id != "")
                                        .Distinct()
                                        .ToArray();
            parents.Text = (rebuilt ? "重建后的父版本：" : "父版本：") +
                           (visible.Length == 0 ? "无父版本" : String.Join("、", visible));
        }
        static string ShortLabel(Record item)
        {
            string text = JobTitle(item).Replace('\r', ' ').Replace('\n', ' ');
            return text.Length <= 55 ? text : text.Substring(0, 55) + "…";
        }
        string Describe(string id)
        {
            var row = rows.FirstOrDefault(r => r.Field("snapshotId") == id);
            if (row == null)
                return id == "" ? "暂无" : id;
            string when = LocalTime(row.Field("capturedAt"));
            return ShortLabel(row) + " · " + (row.Field("outcome") == "fork" ? "分叉" : "主线") + " · " + when +
                   "\n" + id;
        }
        async Task Download()
        {
            var row = Selection;
            if (row == null)
                return;
            using (var picker = new SaveFileDialog { Title = "恢复到新目录",
                                                     FileName = "快照-" + row.Field("snapshotId"),
                                                     CheckFileExists = false, AddExtension = false })
            {
                if (picker.ShowDialog(this) != DialogResult.OK)
                    return;
                await Bridge.Call(null, "restore", unit, row.Field("snapshotId"), "--to", picker.FileName);
                note.Text = "已恢复到 " + picker.FileName;
            }
        }
        async Task Merge()
        {
            var row = Selection;
            if (!Writable || row == null || row.Field("outcome") != "fork")
                return;
            using (var merger = new MergeWorkflow(title, unit, row.Field("snapshotId")))
            {
                if (merger.ShowDialog(this) != DialogResult.OK || String.IsNullOrEmpty(merger.Published))
                    return;
                Changed = true;
                onlyForks.Checked = false;
                preferred = merger.Published;
                await Reload();
                note.Text = "分叉已合并为 " + merger.Published;
            }
        }
        async Task Rollback()
        {
            var row = Selection;
            if (!Writable || row == null || head == "" || head == row.Field("snapshotId"))
                return;
            string selected = row.Field("snapshotId"), observed = head;
            string label = "恢复 · " + JobTitle(row);
            string message = "从此时间点恢复：\n" + Describe(selected) + "\n\n当前 NAS 主线：\n" +
                             Describe(observed) +
                             "\n\n服务器将用所选内容创建新的主线版本。此电脑的工作目录保持原状。";
            using (var dialog =
                       new Form { Text = "确认恢复为当前版本", Font = Style.Type(9), BackColor = Style.Paper,
                                  StartPosition = FormStartPosition.CenterParent,
                                  ClientSize = new Size(600, 340), Padding = new Padding(16) })
            {
                var body = Column();
                dialog.Controls.Add(body);
                Row(body, Occupy(Style.Text(message, 10, false, Style.Ink)), -1);
                Row(body, Occupy(Style.Text("新版本名称", 9, true, Style.Muted)), 26);
                var name = new TextBox { Text = label, Dock = DockStyle.Fill, MaxLength = 500 };
                Row(body, name, 32);
                var buttons = Bar();
                buttons.Controls.Add(Style.Action("确认创建新主线版本", delegate {
                    if (name.Text.Trim().Length > 0)
                        dialog.DialogResult = DialogResult.OK;
                }, true));
                buttons.Controls.Add(
                    Style.Action("取消", delegate { dialog.DialogResult = DialogResult.Cancel; }));
                Row(body, buttons, 0);
                if (dialog.ShowDialog(this) != DialogResult.OK)
                    return;
                label = name.Text.Trim();
            }
            string key = selected + "/" + observed + "/" + label;
            if (restoreKey != key)
            {
                restoreKey = key;
                restoreRequest = Guid.NewGuid().ToString("D");
            }
            var result = await Bridge.Call(null, "timeline", "restore", unit, "--snapshot", selected, "--head",
                                           observed, "--request", restoreRequest, "--name", label);
            string published = result.Field("snapshotId");
            if (published == "")
                throw new InvalidOperationException("暂无新主线版本的回执，请使用相同选择重试。");
            Changed = true;
            onlyForks.Checked = false;
            preferred = published;
            await Reload();
            note.Text = "已恢复为新的当前版本 · " + published;
        }
        static string Parents(Record row, string key)
        {
            var parents = Strings(row, key).ToArray();
            return parents.Length == 0 ? "无父版本" : String.Join("、", parents);
        }
        async Task Remove()
        {
            var row = Selection;
            if (!Writable || row == null || head == "")
                return;
            string selected = row.Field("snapshotId"), observed = head;
            var preview = await Bridge.Call(null, "timeline", "preview-delete", unit, "--snapshot", selected,
                                            "--head", observed);
            if (preview.Field("snapshotId") != selected || preview.Field("expectedHead") != observed)
                throw new InvalidOperationException("删除预览与所选时间点或主线不符，请刷新后重试。");
            var reason = preview.Field("protectedReason");
            var token = preview.Field("planToken");
            if (reason == "" && token == "")
                throw new InvalidOperationException("服务器未提供重建预览凭证，请刷新后重试。");
            object changes;
            if (!preview.TryGetValue("rebuiltPoints", out changes))
                throw new InvalidOperationException("预览缺少重建清单，请重新读取。");
            var rebuilt = changes == null ? new List<Record>() : preview.Rows("rebuiltPoints");
            var detail = new System.Text.StringBuilder();
            foreach (var point in rebuilt)
            {
                detail.AppendLine(JobTitle(point) + "  " + point.Field("snapshotId"));
                detail.AppendLine("原父版本：" + Parents(point, "parentsBefore"));
                detail.AppendLine("重建后父版本：" + Parents(point, "parentsAfter")).AppendLine();
            }
            if (rebuilt.Count == 0)
                detail.Append("其余时间点无需重建。");
            using (var dialog =
                       new Form { Text = "删除并重建 · " + title, Font = Style.Type(9), BackColor = Style.Paper,
                                  StartPosition = FormStartPosition.CenterParent,
                                  ClientSize = new Size(730, 535), MinimumSize = new Size(630, 480),
                                  Padding = new Padding(16) })
            {
                var body = Column();
                dialog.Controls.Add(body);
                Row(body,
                    Occupy(Style.Text("选中的时间点：" + Describe(selected) + "\n当前 NAS 主线：" +
                                          Describe(observed),
                                      10, true, Style.Ink)),
                    104);
                string explanation = reason != "" ? "此时间点受保护：" + reason
                                                  : "此操作将移除所选时间点，并重建其余时间点的父版本关系。逻" +
                                                        "辑删除不会立即释放物理空间。";
                Row(body, Occupy(Style.Text(explanation, 10, false, reason != "" ? Style.Plum : Style.Ink)),
                    55);
                Row(body, Occupy(Style.Text("需要重建的时间点 · " + rebuilt.Count, 10, true, Style.Muted)), 32);
                Row(body,
                    new TextBox { Multiline = true, Dock = DockStyle.Fill, ReadOnly = true, WordWrap = false,
                                  ScrollBars = ScrollBars.Both, BackColor = Style.Canvas,
                                  Text = detail.ToString(), MaxLength = Int32.MaxValue },
                    -1);
                var buttons = Bar();
                if (reason == "")
                    buttons.Controls.Add(Style.Action(
                        "确认删除并重建", delegate { dialog.DialogResult = DialogResult.OK; }, true));
                Button cancel = Style.Action(reason == "" ? "取消" : "关闭",
                                             delegate { dialog.DialogResult = DialogResult.Cancel; });
                buttons.Controls.Add(cancel);
                dialog.CancelButton = cancel;
                dialog.AcceptButton = cancel;
                Row(body, buttons, 0);
                if (dialog.ShowDialog(this) != DialogResult.OK || reason != "")
                    return;
            }
            Record reply;
            for (;;)
            {
                try
                {
                    reply = await Bridge.Call(null, "timeline", "delete", unit, "--snapshot", selected,
                                              "--head", observed, "--plan-token", token);
                    break;
                }
                catch (Exception error)
                {
                    if (error.Message.Contains("HTTP 4"))
                        throw;
                    var retry = MessageBox.Show(this,
                                                "未能取得删除回执。可使用刚才确认的同一凭证再次请求，服务器会" +
                                                "返回原有结果或执行已核对的计划。\n\n" +
                                                    error.Message,
                                                "重新获取删除回执", MessageBoxButtons.RetryCancel,
                                                MessageBoxIcon.Information);
                    if (retry != DialogResult.Retry)
                    {
                        note.Text = "尚未取得删除回执，请刷新核对时间线。";
                        return;
                    }
                }
            }
            if (!String.Equals(reply.Field("deleted"), "true", StringComparison.OrdinalIgnoreCase) ||
                reply.Field("snapshotId") != selected)
                throw new InvalidOperationException("请刷新并核对删除回执。");
            Changed = true;
            preferred = head;
            int count = reply.ContainsKey("rebuiltPoints") && reply["rebuiltPoints"] != null
                            ? reply.Rows("rebuiltPoints").Count
                            : rebuilt.Count;
            await Reload();
            note.Text = "时间点已删除，已重建 " + count + " 个时间点";
        }
        static string PolicyText(string mode, int value)
        {
            if (mode == "all")
                return "保留全部";
            if (mode == "latest")
                return "保留最近 " + value + " 个时间点";
            if (mode == "days")
                return "保留最近 " + value + " 天";
            throw new InvalidOperationException("无法识别服务器的保存策略。");
        }
        static int Nonnegative(Record r, string field)
        {
            int n;
            if (!Int32.TryParse(r.Field(field), NumberStyles.Integer, CultureInfo.InvariantCulture, out n) ||
                n < 0)
                throw new InvalidOperationException("服务器未返回有效的策略数量。");
            return n;
        }
        async Task EditPolicy()
        {
            var loaded = await Bridge.Call(null, "timeline", "policy", unit);
            string oldMode = loaded.Field("mode");
            int oldValue = Nonnegative(loaded, "value");
            string old = PolicyText(oldMode, oldValue);
            string mode;
            int value;
            using (var dialog =
                       new Form { Text = "历史保存策略 · " + title, Font = Style.Type(9),
                                  BackColor = Style.Paper, StartPosition = FormStartPosition.CenterParent,
                                  ClientSize = new Size(570, 265), Padding = new Padding(16) })
            {
                var body = Column();
                dialog.Controls.Add(body);
                Row(body, Occupy(Style.Text("当前策略 · " + old, 12, true, Style.Ink)), 36);
                var options = Bar();
                var choices = Picker(215);
                string[] codes = { "all", "latest", "days" };
                choices.Items.AddRange(new object[] { "保留全部", "按最近时间点数", "按天数" });
                var amount = new NumericUpDown { Minimum = 1, Maximum = 10000, Value = 1, Width = 90 };
                var suffix = Hint("");
                choices.SelectedIndexChanged += delegate
                {
                    int i = choices.SelectedIndex;
                    amount.Maximum = i == 2 ? 3650 : 10000;
                    amount.Visible = suffix.Visible = i != 0;
                    suffix.Text = i == 1 ? "个时间点（1–10000）" : "天（1–3650）";
                };
                choices.SelectedIndex = Array.IndexOf(codes, oldMode);
                amount.Value = Math.Max(1, Math.Min((int)amount.Maximum, oldValue));
                options.Controls.Add(choices);
                options.Controls.Add(amount);
                options.Controls.Add(suffix);
                options.Visible = Writable;
                Row(body, options, 0);
                string explanation = "当前主线、未合并分叉和被副本持有的版本受保护。策略隐藏超出范围的时间点" +
                                     "，逻辑删除不会立即释放物理空间。";
                if (!Writable)
                    explanation += "\n此设备有读取权限，可查看当前策略。";
                Row(body, Occupy(Style.Text(explanation, 10, false, Style.Muted)), -1);
                var buttons = Bar();
                if (Writable)
                    buttons.Controls.Add(
                        Style.Action("预览影响…", delegate { dialog.DialogResult = DialogResult.OK; }, true));
                buttons.Controls.Add(
                    Style.Action("关闭", delegate { dialog.DialogResult = DialogResult.Cancel; }));
                Row(body, buttons, 0);
                if (dialog.ShowDialog(this) != DialogResult.OK || !Writable)
                    return;
                mode = codes[choices.SelectedIndex];
                value = mode == "all" ? 0 : (int)amount.Value;
            }
            if (mode == oldMode && value == oldValue)
            {
                note.Text = "历史保存策略没有变化";
                return;
            }
            string n = value.ToString(CultureInfo.InvariantCulture);
            string observed = head;
            var preview =
                await Bridge.Call(null, "timeline", "preview-policy", unit, "--mode", mode, "--value", n);
            if (preview.Field("mode") != mode || Nonnegative(preview, "value") != value)
                throw new InvalidOperationException("预览与所选策略不一致，请重新读取。");
            int count = Nonnegative(preview, "removeCount"),
                protectedCount = Nonnegative(preview, "protectedCount");
            string prompt = "资料库：" + title + "\n原策略：" + old + "\n新策略：" + PolicyText(mode, value) +
                            "\n\n预计隐藏 " + count + " 个时间点，额外保护 " + protectedCount +
                            " 个。\n已隐藏的时间点不会因延长保存期限重新出现，物理空间不会立即释放。";
            if (MessageBox.Show(this, prompt, "确认应用历史保存策略", MessageBoxButtons.YesNo,
                                MessageBoxIcon.Warning, MessageBoxDefaultButton.Button2) != DialogResult.Yes)
                return;
            var args = new List<string> { "timeline", "policy", unit, "--mode", mode, "--value", n };
            if (observed != "")
            {
                args.Add("--head");
                args.Add(observed);
            }
            var result = await Bridge.Call(null, args.ToArray());
            Changed = true;
            if (result.Field("mode") != mode || Nonnegative(result, "value") != value)
                throw new InvalidOperationException("未收到预期的策略回执，请刷新后重新核对。");
            await Reload();
            note.Text = "保存策略已更新 · " + PolicyText(mode, value);
        }
    }
}
}
