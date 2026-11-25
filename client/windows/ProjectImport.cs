using System;
using System.Collections.Generic;
using System.Drawing;
using System.Linq;
using System.Threading.Tasks;
using System.Windows.Forms;

namespace XinhuSync
{
internal sealed class NasTarget
{
    public readonly string Scope, Space, Unit, Name, SourceKey;
    public bool IsNew
    {
        get {
            return Unit.Length == 0;
        }
    }
    static string Short(string value)
    {
        return value.Length > 8 ? value.Substring(value.Length - 8) : value;
    }
    public NasTarget(string scope, string space, string unit, string name, string sourceKey = "")
    {
        Scope = scope;
        Space = space;
        Unit = unit;
        Name = name;
        SourceKey = sourceKey;
    }
    public override string ToString()
    {
        return IsNew ? "新建独立项目 · " + Space + " [" + Short(Scope) + "]"
                     : Name + " · " + Space + " [" + Short(Unit) + "]";
    }
    public string Destination(string localName)
    {
        return IsNew ? Space + " / " + localName + "（新建独立项目）"
                     : Space + " / " + Name + " [" + Short(Unit) + "]";
    }
}
internal sealed class ImportProject
{
    public string Name, Path, Kind, Origin, SourceKey, Outcome, OldUnit, OldName;
    public NasTarget Target;
    public readonly string Request = Guid.NewGuid().ToString("D");
    public bool ConfirmRebind, HasPending;
    public bool IsRebind
    {
        get {
            return !String.IsNullOrEmpty(OldUnit) && Target != null && Target.Unit != OldUnit;
        }
    }
    public List<string> ImportArguments()
    {
        if (Target == null)
            throw new InvalidOperationException("请选择要关联的 NAS 项目，或者选择新建独立项目。");
        if (IsRebind && (HasPending || !ConfirmRebind))
            throw new InvalidOperationException("请先上传原绑定的待上传快照，再确认重新关联。");
        var args = new List<string> { "project", "import", Target.Scope, "--path", Path, "--name", Name };
        if (Target.IsNew)
            args.AddRange(new[] { "--new", "--request", Request });
        else
            args.AddRange(new[] { "--existing", Target.Unit });
        if (IsRebind)
            args.AddRange(new[] { "--rebind-from", OldUnit });
        return args;
    }
    public bool Included;
    public List<Record> Ignored = new List<Record>();
    public bool IncludeAll = true, PolicyChanged, PolicyLoaded;
    public bool Includes(string path) { return IncludeAll != PolicyPaths.Contains(path); }
    public void SetIncluded(string path, bool value)
    {
        if (value == IncludeAll) PolicyPaths.Remove(path); else PolicyPaths.Add(path);
        PolicyChanged = true;
    }
    public HashSet<string> PolicyPaths = new HashSet<string>(StringComparer.Ordinal);
    public override string ToString()
    {
        return Name;
    }
}

// Per-project settings remain with their project when the user changes selection.
internal sealed class ProjectImport : Form
{
    readonly List<ImportProject> projects;
    readonly Func<ImportProject, Task> apply;
    readonly ListView candidates = Table("本机项目", "NAS 目标"), ignored = Table("忽略项", "大小");
    readonly ComboBox library =
        new ComboBox { DropDownStyle = ComboBoxStyle.DropDownList, Dock = DockStyle.Fill };
    readonly Button allIgnored, noIgnored;
    readonly Button scan;
    readonly Label association = Style.Text("", 9, false, Style.Muted);
    readonly CheckBox rebind = new CheckBox { Text = "确认重新关联，保留原 NAS 项目的历史", AutoSize = true,
                                              Anchor = AnchorStyles.Left };
    readonly Label path = Style.Text("", 9, false, Style.Muted),
                   summary = Style.Text("", 9, false, Style.Muted);
    readonly Panel details = new Panel { Dock = DockStyle.Fill };
    readonly Button import;
    ImportProject current;
    bool loading, working;
    readonly bool editing;
    public int Completed
    {
        get {
            return projects.Count(p => p.Outcome != null);
        }
    }

    static ListView Table(params string[] columns)
    {
        var v = new ListView { Dock = DockStyle.Fill,   View = View.Details,
                               CheckBoxes = true,       FullRowSelect = true,
                               MultiSelect = false,     HideSelection = false,
                               BackColor = Style.Paper, BorderStyle = BorderStyle.FixedSingle };
        foreach (string name in columns)
            v.Columns.Add(name);
        v.Resize += delegate
        {
            int available = Math.Max(145, v.ClientSize.Width - SystemInformation.VerticalScrollBarWidth - 3);
            v.Columns[0].Width = available - 83;
            v.Columns[1].Width = 83;
        };
        return v;
    }

    static Label Label(string text)
    {
        var l = Style.Text(text, 9, true, Style.Ink);
        l.Dock = DockStyle.Fill;
        return l;
    }

    public ProjectImport(string root, List<ImportProject> options, List<NasTarget> targets,
                         Func<ImportProject, Task> submit, bool editSelection = false)
    {
        editing = editSelection;
        projects = options;
        apply = submit;
        Text = editing ? "管理 Git 忽略项" : "导入项目父目录";
        Font = Style.Type(9);
        BackColor = Style.Canvas;
        ForeColor = Style.Ink;
        AutoScaleDimensions = new SizeF(96, 96);
        AutoScaleMode = AutoScaleMode.Dpi;
        StartPosition = FormStartPosition.CenterParent;
        MinimumSize = new Size(780, 550);
        ClientSize = new Size(930, 605);
        var layout = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 2, RowCount = 3,
                                            Padding = new Padding(18) };
        layout.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 40));
        layout.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 60));
        layout.RowStyles.Add(new RowStyle(SizeType.Absolute, 47));
        layout.RowStyles.Add(new RowStyle(SizeType.Percent, 100));
        layout.RowStyles.Add(new RowStyle(SizeType.Absolute, 53));
        var title = Style.Text(editing ? "Git 忽略文件备份范围" : "选择要导入的项目", 15, true, Style.Ink);
        title.Dock = DockStyle.Fill;
        layout.Controls.Add(title, 0, 0);
        var chosenRoot = Style.Text(root, 9, false, Style.Muted);
        chosenRoot.Dock = DockStyle.Fill;
        layout.Controls.Add(chosenRoot, 1, 0);
        layout.Controls.Add(candidates, 0, 1);
        layout.Controls.Add(details, 1, 1);
        var editor = new TableLayoutPanel { Dock = DockStyle.Fill, Padding = new Padding(12, 0, 0, 0),
                                            ColumnCount = 1, RowCount = 7 };
        editor.RowStyles.Add(new RowStyle(SizeType.Absolute, 46));
        editor.RowStyles.Add(new RowStyle(SizeType.Absolute, 25));
        editor.RowStyles.Add(new RowStyle(SizeType.Absolute, 34));
        editor.RowStyles.Add(new RowStyle(SizeType.Absolute, 25));
        editor.RowStyles.Add(new RowStyle(SizeType.Absolute, 34));
        editor.RowStyles.Add(new RowStyle(SizeType.Absolute, 42));
        editor.RowStyles.Add(new RowStyle(SizeType.Percent, 100));
        path.Dock = DockStyle.Fill;
        editor.Controls.Add(path, 0, 0);
        editor.Controls.Add(Label("关联已有 NAS 项目，或新建独立项目"), 0, 1);
        editor.Controls.Add(library, 0, 2);
        association.Dock = DockStyle.Fill;
        editor.Controls.Add(association, 0, 3);
        editor.Controls.Add(rebind, 0, 4);
        var filters =
            new FlowLayoutPanel { Dock = DockStyle.Fill, WrapContents = false, Margin = Padding.Empty };
        filters.Controls.Add(new Label { AutoSize = true, Text = "Git 忽略文件", Margin = new Padding(0, 8, 5, 0) });
        scan = Style.Action("查看文件", async delegate { await LoadIgnored(); });
        allIgnored = Style.Action("全选", delegate { SetAll(true); });
        noIgnored = Style.Action("清空选择", delegate { SetAll(false); });
        filters.Controls.Add(scan);
        filters.Controls.Add(allIgnored);
        filters.Controls.Add(noIgnored);
        editor.Controls.Add(filters, 0, 5);
        editor.Controls.Add(ignored, 0, 6);
        details.Controls.Add(editor);
        var buttons = new FlowLayoutPanel { Dock = DockStyle.Fill, FlowDirection = FlowDirection.RightToLeft,
                                            Padding = new Padding(0, 7, 0, 0) };
        import = Style.Action(editing ? "保存选择" : "导入所选项目", async delegate { await ApplySelected(); },
                              true);
        if (editing)
        {
            layout.Controls.Remove(candidates);
            Controls.Add(candidates);
            candidates.Visible = false;
            layout.ColumnStyles[0].Width = 26;
            layout.Controls.Remove(details);
            layout.Controls.Add(details, 0, 1);
            layout.SetColumnSpan(details, 2);
            for (int row = 1; row <= 4; row++)
            {
                editor.RowStyles[row].Height = 0;
                editor.GetControlFromPosition(0, row).Visible = false;
            }
        }
        var close = Style.Action("关闭", delegate { Close(); });
        buttons.Controls.Add(import);
        buttons.Controls.Add(close);
        if (!editing)
        {
            var all = Style.Action("全选", delegate {
                foreach (ListViewItem entry in candidates.Items)
                    entry.Checked = true;
            });
            buttons.Controls.Add(all);
        }
        summary.Dock = DockStyle.Fill;
        layout.Controls.Add(summary, 0, 2);
        layout.Controls.Add(buttons, 1, 2);
        Controls.Add(layout);
        library.Items.Add("请选择 NAS 项目…");
        foreach (var target in targets)
            library.Items.Add(target);
        library.DropDownWidth = 580;
        foreach (var p in projects)
            candidates.Items.Add(new ListViewItem(
                new[] { p.Name, TargetLabel(p) }) { Tag = p, Checked = p.Included, ToolTipText = p.Path });
        candidates.ShowItemToolTips = true;
        candidates.ItemCheck += delegate(object sender, ItemCheckEventArgs e)
        {
            var p = (ImportProject)candidates.Items[e.Index].Tag;
            if (working || p.Outcome != null)
            {
                e.NewValue = e.CurrentValue;
                return;
            }
            p.Included = e.NewValue == CheckState.Checked;
            RefreshSummary();
        };
        ignored.ItemCheck += delegate(object sender, ItemCheckEventArgs e)
        {
            if (loading || working || current == null || current.Outcome != null)
                return;
            string key = (string)ignored.Items[e.Index].Tag;
            current.SetIncluded(key, e.NewValue == CheckState.Checked);
            RefreshSummary();
        };
        candidates.SelectedIndexChanged += delegate
        {
            if (candidates.SelectedItems.Count > 0)
                SelectProject((ImportProject)candidates.SelectedItems[0].Tag);
        };
        library.SelectedIndexChanged += delegate
        {
            if (loading || current == null)
                return;
            current.Target = library.SelectedItem as NasTarget;
            current.ConfirmRebind = false;
            SelectProject(current);
            RefreshSummary();
            var entry = candidates.Items.Cast<ListViewItem>().First(i => i.Tag == current);
            entry.SubItems[1].Text = TargetLabel(current);
        };
        rebind.CheckedChanged += delegate
        {
            if (!loading && current != null)
                current.ConfirmRebind = rebind.Checked;
        };
        FormClosing += delegate(object sender, FormClosingEventArgs e)
        {
            if (working && e.CloseReason == CloseReason.UserClosing)
                e.Cancel = true;
        };
        if (projects.Count > 0)
        {
            candidates.Items[0].Selected = true;
            SelectProject(projects[0]);
        }
        else
            details.Enabled = false;
        RefreshSummary();
        if (editing) Shown += async delegate { await LoadIgnored(); };
    }

    static string TargetLabel(ImportProject p)
    {
        if (p.Target == null)
            return "请选择";
        return p.Target.IsNew ? "新建独立项目" : p.Target.Name;
    }
    void RefreshSummary()
    {
        int selected = projects.Count(p => p.Included && p.Outcome == null);
        string scope = current == null || current.Kind != "git" ? "" :
            !current.PolicyLoaded && !String.IsNullOrEmpty(current.OldUnit) ? "沿用项目已保存的备份范围" :
            current.IncludeAll ? "备份全部忽略文件（含新增），排除 " + current.PolicyPaths.Count + " 项" :
            "仅备份勾选的 " + current.PolicyPaths.Count + " 项";
        summary.Text = editing ? scope : String.Format("项目 {0} / {1}  ·  已完成 {2}\n{3}", selected,
                                                         projects.Count, Completed, scope);
        import.Enabled = !working && selected > 0;
    }
    void SetAll(bool value)
    {
        if (current == null || current.Kind != "git") return;
        current.IncludeAll = value;
        current.PolicyPaths.Clear();
        current.PolicyLoaded = current.PolicyChanged = true;
        SelectProject(current);
    }
    void SelectProject(ImportProject p)
    {
        loading = true;
        try
        {
            current = p;
            path.Text = p.Path + (String.IsNullOrEmpty(p.Origin) ? "" : "\n" + p.Origin);
            library.SelectedIndex = p.Target == null ? 0 : Math.Max(0, library.Items.IndexOf(p.Target));
            association.Text = p.IsRebind && p.HasPending ? "原项目有待上传快照，上传完成后才能重新关联。"
                               : String.IsNullOrEmpty(p.OldUnit) ? "本机尚未关联 NAS 项目"
                                                                 : "当前关联：" + p.OldName;
            if (p.Target == null && String.IsNullOrEmpty(p.OldUnit) && !String.IsNullOrEmpty(p.SourceKey))
            {
                var same = library.Items.OfType<NasTarget>()
                               .Where(t => !t.IsNew && t.SourceKey == p.SourceKey)
                               .ToList();
                if (same.Count == 1)
                    association.Text = "同一 Git 来源的项目：" + same[0];
                if (same.Count > 1)
                    association.Text = "有 " + same.Count + " 个项目使用同一 Git 来源，请选择所需项目。";
            }
            rebind.Visible = !editing && p.IsRebind;
            rebind.Enabled = !p.HasPending;
            rebind.Checked = p.ConfirmRebind;
            ignored.BeginUpdate();
            ignored.Items.Clear();
            foreach (var r in p.Ignored)
            {
                string rel = r.Field("path");
                ignored.Items.Add(new ListViewItem(new[] { rel, MainWindow.FormatBytes(r.Field("size")) }) {
                    Tag = rel, Checked = p.Includes(rel)
                });
            }
            RefreshSummary();
            scan.Enabled = allIgnored.Enabled = noIgnored.Enabled = String.Equals(p.Kind, "git", StringComparison.OrdinalIgnoreCase);
            details.Enabled = !working && p.Outcome == null;
        }
        finally
        {
            ignored.EndUpdate();
            loading = false;
        }
    }
    async Task LoadIgnored()
    {
        var p = current;
        if (p == null || working)
            return;
        scan.Enabled = false;
        try
        {
            if (!p.PolicyLoaded && !String.IsNullOrEmpty(p.OldUnit))
            {
                var saved = await Bridge.Call(null, "project", "includes", p.OldUnit);
                if (!p.PolicyChanged) MainWindow.LoadIgnorePolicy(p, saved);
            }
            var rows = (await Bridge.Call(null, "project", "ignored", "--path", p.Path, "--min-bytes","0")).Rows();
            var visible = new HashSet<string>(rows.Select(v => v.Field("path")), StringComparer.Ordinal);
            foreach (var key in p.PolicyPaths.Where(v => !visible.Contains(v)))
                rows.Add(new Record(new Dictionary<string, object> { { "path", key } }));
            p.Ignored = rows;
            if (current == p && !IsDisposed)
            {
                SelectProject(p);
                RefreshSummary();
            }
        }
        catch (Exception ex)
        {
            if (!IsDisposed)
                MessageBox.Show(this, ex.Message, "读取 Git 忽略项");
        }
        finally
        {
            if (!IsDisposed && current != null)
                scan.Enabled = String.Equals(current.Kind, "git", StringComparison.OrdinalIgnoreCase);
        }
    }
    bool ConfirmDestinations(List<ImportProject> list)
    {
        using (var f = new Form { Text = "确认项目关联", StartPosition = FormStartPosition.CenterParent,
                                  Size = new Size(850, 420), MinimumSize = new Size(560, 340),
                                  Padding = new Padding(14), Font = Style.Type(9) })
        {
            var grid = Table("本机目录", "NAS 项目");
            grid.CheckBoxes = false;
            grid.ShowItemToolTips = true;
            grid.Resize += delegate
            {
                int total = Math.Max(120, grid.ClientSize.Width - SystemInformation.VerticalScrollBarWidth - 4);
                grid.Columns[0].Width = total / 2;
                grid.Columns[1].Width = total - total / 2;
            };
            foreach (var p in list)
            {
                var destination = p.Target.Destination(p.Name);
                grid.Items.Add(new ListViewItem(new[] { p.Path, destination }) {
                    ToolTipText = p.Path + " → " + destination + (p.IsRebind ? "（原历史保留）" : "")
                });
            }
            var buttons = new FlowLayoutPanel { Dock = DockStyle.Bottom, Height = 42,
                                                FlowDirection = FlowDirection.RightToLeft };
            var ok = Style.Action("确认导入", delegate { f.DialogResult = DialogResult.OK; }, true);
            buttons.Controls.Add(ok);
            var back = Style.Action("返回修改", delegate { f.DialogResult = DialogResult.Cancel; });
            buttons.Controls.Add(back);
            f.Controls.Add(grid);
            f.Controls.Add(buttons);
            f.CancelButton = back;
            return f.ShowDialog(this) == DialogResult.OK;
        }
    }
    async Task ApplySelected()
    {
        if (working)
            return;
        var list = projects.Where(p => p.Included && p.Outcome == null).ToList();
        if (list.Count == 0)
            return;
        if (!editing)
        {
            var missing = list.FirstOrDefault(p => p.Target == null ||
                                                   (p.IsRebind && (p.HasPending || !p.ConfirmRebind)));
            if (missing != null)
            {
                var row = candidates.Items.Cast<ListViewItem>().First(i => i.Tag == missing);
                row.Selected = true;
                row.EnsureVisible();
                SelectProject(missing);
                MessageBox.Show(this, missing.Target == null ? "请为「" + missing.Name + "」选择目标。"
                                      : missing.HasPending   ? "原项目仍有待上传快照。"
                                                             : "请勾选重新关联确认。");
                return;
            }
            var duplicate =
                list.Where(p => !p.Target.IsNew).GroupBy(p => p.Target.Unit).FirstOrDefault(g => g.Count() > 1);
            if (duplicate != null)
            {
                MessageBox.Show(this, "同一台电脑上，一个 NAS 项目只能绑定一个本地目录。请核对 " +
                                          duplicate.First().Target.Name + "。");
                return;
            }
            if (!ConfirmDestinations(list))
                return;
        }
        working = true;
        details.Enabled = false;
        candidates.Enabled = false;
        import.Enabled = false;
        try
        {
            foreach (var p in list)
            {
                var item = candidates.Items.Cast<ListViewItem>().First(i => i.Tag == p);
                item.SubItems[1].Text = "正在导入";
                try
                {
                    await apply(p);
                    p.Outcome = editing ? "已保存" : "已绑定";
                    item.SubItems[1].Text = p.Outcome;
                }
                catch (Exception ex)
                {
                    item.SubItems[1].Text = "待重试";
                    item.ToolTipText = ex.Message;
                    MessageBox.Show(this, p.Name + "\n" + ex.Message, "导入中断", MessageBoxButtons.OK,
                                    MessageBoxIcon.Information);
                    break;
                }
            }
        }
        finally
        {
            working = false;
            candidates.Enabled = true;
            if (current != null)
                SelectProject(current);
            RefreshSummary();
        }
        if (list.All(p => p.Outcome != null))
            DialogResult = DialogResult.OK;
    }
}
}
