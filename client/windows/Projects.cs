using System;
using System.Collections.Generic;
using System.Drawing;
using System.IO;
using System.Linq;
using System.Threading.Tasks;
using System.Windows.Forms;
using System.Web.Script.Serialization;

namespace XinhuSync
{
internal static class ProjectDisplay
{
    public static string Nas(Record r)
    {
        switch (r.Field("nasState"))
        {
        case "server_only":
            return "仅在 NAS";
        case "empty":
            return "尚无版本";
        case "local_only":
            return "仅本机";
        case "pending":
            return "待上传";
        case "current":
            return "已同步";
        case "protected":
            return "主线受保护";
        case "baseline":
            return "已绑定基线";
        case "remote_ahead":
            return "NAS 有新版";
        case "fork":
            return "已保留分叉";
        case "diverged":
            return "双端有改动";
        default:
            return "待核对";
        }
    }
    public static string Git(Record r)
    {
        if (!String.Equals(r.Field("kind"), "git", StringComparison.OrdinalIgnoreCase))
            return "—";
        object data;
        if (!r.TryGetValue("git", out data) || !(data is IDictionary<string, object>))
            return String.IsNullOrEmpty(r.Field("gitError")) ? "待检查" : "检查失败";
        var v = new Record((IDictionary<string, object>)data);

        var state = v.Field("state");
        string text;
        switch (state)
        {
        case "equal":
            text = "一致";
            break;
        case "ahead":
            text = "本地领先";
            break;
        case "behind":
            text = "上游领先";
            break;
        case "diverged":
            text = "双方有提交";
            break;
        case "no_upstream":
            text = "无上游";
            break;
        case "unborn":
            text = "暂无提交";
            break;
        case "unknown":
        case "":
            text = "待核对";
            break;
        case "remote_changed":
            text = "Git 远端有变化";
            break;
        case "offline":
            text = "上游离线";
            break;
        case "upstream_missing":
            text = "找不到上游";
            break;
        case "detached":
            text = "分离 HEAD";
            break;
        case "not_git":
            text = "—";
            break;
        case "unavailable":
        case "fetch_failed":
        case "error":
            text = "检查失败";
            break;
        default:
            text = state;
            break;
        }
        int ahead, behind;
        if (Int32.TryParse(v.Field("ahead"), out ahead) && ahead > 0)
            text += " ↑" + ahead;
        if (Int32.TryParse(v.Field("behind"), out behind) && behind > 0)
            text += " ↓" + behind;
        if (String.Equals(v.Field("dirty"), "true", StringComparison.OrdinalIgnoreCase))
            text += " · 未提交";
        if (!String.Equals(v.Field("fetched"), "true", StringComparison.OrdinalIgnoreCase) &&
            (state == "equal" || state == "behind" || state == "ahead" || state == "diverged"))
            text += "（本地引用）";
        if (String.Equals(v.Field("fetched"), "true", StringComparison.OrdinalIgnoreCase))
            text += " · 已核对上游";
        if (!String.IsNullOrWhiteSpace(r.Field("gitError")))
            text += " · 本次检查失败";
        return text;
    }
}

internal sealed partial class MainWindow
{
    ProjectView projectView;
    string selectedProject;
    Record SelectedProject()
    {
        var selected = projectView.Selected;
        if (selected == null) throw new InvalidOperationException("请先选择项目。");
        return selected;
    }
    Control ProjectsPage()
    {
        projectView = new ProjectView(async delegate { await Run(ImportProjects); },
                                      async delegate { await Run(OpenProjectLibrary); }, LocalTime);
        projectView.AddAction("版本时间线…", async delegate { await Run(ProjectTimeline); });
        projectView.AddAction("接收 NAS 主线到新目录…", async delegate { await Run(CheckoutProject); });
        projectView.AddSeparator();
        projectView.AddAction("检查 Git 上游…", async delegate { await Run(CheckProjectGit); }, true);
        projectView.AddAction("同步远端 Git…", async delegate { await Run(SyncGitRemote); }, true);
        projectView.AddAction("管理 Git 忽略项…", async delegate { await Run(EditIncludes); }, true);
        projectView.AddSeparator();
        projectView.AddAction("移除本机记录…", async delegate {
            await Run(delegate { return RemoveLibrary("forget", SelectedProject().Field("unitId")); });
        });
        projectView.AddAction("从 NAS 删除…", async delegate {
            await Run(delegate { return RemoveLibrary("delete", SelectedProject().Field("unitId")); });
        });
        return new System.Windows.Forms.Integration.ElementHost {
            Child = projectView, Dock = DockStyle.Fill, Margin = Padding.Empty
        };
    }
    async Task CheckProjectGit()
    {
        var project = SelectedProject();
        if (project.Field("kind") != "git" || String.IsNullOrEmpty(project.Field("path")))
            throw new InvalidOperationException("请选择本机已绑定的 Git 项目。");
        if (MessageBox.Show(this, "将重新核对 Git 上游。为比较分支历史，可能从远端取回本机缺少的 Git 对象。",
                            "深度检查 " + project.Field("name"), MessageBoxButtons.OKCancel,
                            MessageBoxIcon.Information) != DialogResult.OK)
            return;
        var fresh = await Bridge.Call(null, "project", "git", "--path", project.Field("path"), "--fetch");
        shell.ApplyGitState(project.Field("unitId"), fresh);
        RenderProjects();
    }
    void RenderProjects()
    {
        projectView.SetProjects(shell.Projects, shell.ProjectError, selectedProject);
        if (selectedProject != null && projectView.Selected != null && projectView.Selected.Field("unitId") == selectedProject)
            selectedProject = null;
    }
    public void SelectProject(string id)
    {
        selectedProject = id;
        ShowSection("projects");
    }
    async Task ImportProjects()
    {
        string root;
        using (var pick = new FolderBrowserDialog { Description = "选择包含多个项目的父目录",
                                                    ShowNewFolderButton = false })
        {
            if (pick.ShowDialog(this) != DialogResult.OK)
                return;
            root = pick.SelectedPath;
        }
        var discovery = await Bridge.Call(null, "project", "discover", "--path", root);
        var candidates =
            discovery.Rows()
                .Select(r => new ImportProject { Name = r.Field("name"), Path = r.Field("path"),
                                                 Kind = r.Field("kind"), Origin = r.Field("origin"),
                                                 SourceKey = r.Field("sourceKey") })
                .ToList();
        string warnings = String.Join("\n", Strings(discovery, "warnings"));
        if (warnings.Length > 0)
            MessageBox.Show(this, warnings, "扫描提示", MessageBoxButtons.OK);
        if (candidates.Count == 0)
        {
            status.Text = "没有发现可导入的子项目";
            return;
        }
        var allUnits = (await Bridge.Call(null, "units")).Rows();
        var scopes = allUnits
                         .Where(r => r.Field("role") == "sync" && r.Field("kind") == "managed" &&
                                     String.IsNullOrEmpty(r.Field("scopeId")))
                         .ToList();
        if (scopes.Count == 0)
            throw new InvalidOperationException("需要先配对并获得一个备份空间的同步授权。");
        var targets = new List<NasTarget>();
        foreach (var space in scopes)
        {
            string scope = space.Field("unitId"), name = space.Field("name");
            foreach (var project in (await Bridge.Call(null, "project", "list", scope)).Rows())
            {
                string originKey = project.Field("sourceKey");
                if (originKey == "" && project.Field("key").StartsWith("git:", StringComparison.Ordinal))
                    originKey = project.Field("key");
                targets.Add(
                    new NasTarget(scope, name, project.Field("unitId"), project.Field("name"), originKey));
            }
            targets.Add(new NasTarget(scope, name, "", ""));
        }
        var bindings = (await Bridge.Call(null, "bindings")).Rows();
        var queued = new HashSet<string>((await Bridge.Call(null, "queue"))
                                             .Rows()
                                             .Where(r => r.Field("state") == "queued")
                                             .Select(r => r.Field("unitId")));
        foreach (var p in candidates)
        {
            var prior = bindings.FirstOrDefault(
                binding => String.Equals(Path.GetFullPath(binding.Field("path")), Path.GetFullPath(p.Path),
                                         StringComparison.OrdinalIgnoreCase));
            if (prior == null)
                continue;
            p.OldUnit = prior.Field("unitId");
            p.HasPending = queued.Contains(p.OldUnit);
            p.Target = targets.FirstOrDefault(t => !t.IsNew && t.Unit == p.OldUnit);
            var old = allUnits.FirstOrDefault(r => r.Field("unitId") == p.OldUnit);
            p.OldName = p.Target != null ? p.Target.Destination(p.Name)
                        : old != null    ? old.Field("name")
                                         : p.OldUnit;
        }
        using (var f = new ProjectImport(
                   root, candidates, targets,
                   async p =>
                   {
                       var args = p.ImportArguments();
                       if (!p.Target.IsNew)
                       {
                           var live = (await Bridge.Call(null, "project", "list", p.Target.Scope)).Rows();
                           var match = live.FirstOrDefault(r => r.Field("unitId") == p.Target.Unit);
                           if (match == null)
                               throw new InvalidOperationException(
                                   "所选 NAS 项目已不在授权清单中，请重新打开导入。");
                           string head = match.Field("head");
                           if (!String.IsNullOrEmpty(head))
                           {
                               if (MessageBox.Show(Form.ActiveForm,
                                                   "本地目录：" + p.Path + "\nNAS 项目：" +
                                                       p.Target.Destination(p.Name) + "\n当前版本：" + head +
                                                       "\n\n使用这个 NAS 版本作为本机项目的基线？",
                                                   "核对已有 NAS 版本", MessageBoxButtons.YesNo,
                                                   MessageBoxIcon.Question) != DialogResult.Yes)
                                   throw new InvalidOperationException("已取消接续该版本。");
                               args.Add("--base");
                               args.Add(head);
                           }
                       }
                       if (p.PolicyChanged) await CallWithSelection(args, p);
                       else await Bridge.Call(null, args.ToArray());
                   }))
        {
            f.ShowDialog(this);
            if (f.Completed > 0)
            {
                await shell.RefreshProjects(false);
                RenderProjects();
                status.Text = "已绑定 " + f.Completed + " 个项目";
            }
        }
    }
    static Task<Record> CallWithSelection(List<string> args, ImportProject project)
    {
        args.AddRange(new[] { "--ignore-mode", project.IncludeAll ? "all" : "selected", "--includes-stdin" });
        var json = new JavaScriptSerializer { MaxJsonLength = Int32.MaxValue };
        return Bridge.Call(json.Serialize(project.PolicyPaths.OrderBy(p => p, StringComparer.Ordinal).ToArray()),
                           args.ToArray());
    }
    static IEnumerable<string> Strings(Record r, string key)
    {
        object value;
        if (!r.TryGetValue(key, out value) || value == null || value is string ||
            !(value is System.Collections.IEnumerable))
            return Enumerable.Empty<string>();
        return ((System.Collections.IEnumerable)value).Cast<object>().Select(x => Convert.ToString(x));
    }
    async Task OpenProjectLibrary()
    {
        var id = SelectedProject().Field("unitId");
        await RefreshConnection();
        var item = units.Items.Cast<Choice>().FirstOrDefault(x => x.Row.Field("unitId") == id);
        if (item == null)
            throw new InvalidOperationException("当前设备缺少该资料库的读取授权。");
        rebuilding = true;
        try
        {
            units.SelectedItem = item;
        }
        finally
        {
            rebuilding = false;
        }
        DisplayPage("libraries");
        await SelectUnit();
    }
    async Task CheckoutProject()
    {
        var r = SelectedProject();
        using (var pick = new SaveFileDialog { Title = "新工作树目录（必须尚不存在）", Filter = "目录名称|*.*",
                                               CheckFileExists = false, AddExtension = false,
                                               FileName = r.Field("name") + "-NAS" })
        {
            if (pick.ShowDialog(this) != DialogResult.OK)
                return;
            if (Directory.Exists(pick.FileName) || File.Exists(pick.FileName))
                throw new InvalidOperationException("请选择尚不存在的新目录。");
            var message =
                (String.IsNullOrEmpty(r.Field("path")) ? "" : "原工作树保留在 " + r.Field("path") + "。\n") +
                "将 NAS 主线恢复到 " + pick.FileName +
                " 并绑定这个目录。快照包含工作树文件，不含 .git 数据库。";
            if (MessageBox.Show(this, message, "确认切换工作树", MessageBoxButtons.OKCancel) != DialogResult.OK)
                return;
            await Bridge.Call(null, "project", "checkout", r.Field("unitId"), "--to", pick.FileName);
        }
        await shell.RefreshProjects(false);
        RenderProjects();
        status.Text = "已切换到 NAS 主线工作树";
    }
    internal static void LoadIgnorePolicy(ImportProject item, Record saved)
    {
        var mode = saved.Field("mode");
        if (mode != "all" && mode != "selected") throw new InvalidDataException("无法识别 Git 忽略文件策略。");
        item.IncludeAll = mode == "all";
        item.PolicyPaths = new HashSet<string>(Strings(saved,"items"), StringComparer.Ordinal);
        item.PolicyLoaded = true;
    }
    async Task EditIncludes()
    {
        var r = SelectedProject();
        if (r.Field("kind") != "git")
            throw new InvalidOperationException("该操作仅适用于 Git 项目。");
        string id = r.Field("unitId");
        var item = new ImportProject { Name = r.Field("name"), Kind = r.Field("kind"), Path = r.Field("path"),
                                       Included = true, OldUnit = id };
        LoadIgnorePolicy(item, await Bridge.Call(null, "project", "includes", id));
        using (var editor =
                   new ProjectImport(item.Path, new List<ImportProject> { item }, new List<NasTarget>(),
                                     async edited =>
                                     {
                                         var args = new List<string> { "project", "includes", id, "--set" };
                                         await CallWithSelection(args, edited);
                                     },
                                     true))
        {
            editor.Text = "管理 Git 忽略项";
            editor.ShowDialog(this);
            if (editor.Completed > 0)
                status.Text = "Git 忽略项选择已保存";
        }
    }
}
}
