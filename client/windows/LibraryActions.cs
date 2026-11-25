using System;
using System.Collections.Generic;
using System.Drawing;
using System.Threading.Tasks;
using System.Windows.Forms;

namespace XinhuSync
{
internal sealed partial class MainWindow
{
    async Task SyncGitRemote()
    {
        var project = Selected(projectGrid);
        string path = project.Field("path");
        if (project.Field("kind") != "git" || path.Length == 0)
            throw new InvalidOperationException("请选择本机已绑定的 Git 项目。");
        var args = new List<string> { "project", "git-sync", "--path", path };
        var plan = await Bridge.Call(null, args.ToArray());
        var state = plan.Field("state");
        shell.ApplyGitState(project.Field("unitId"), plan);
        string detail = plan.Field("branch") + " → " + plan.Field("upstream") + "\n" + path + "\n\n";
        if (state == "ahead" || state == "behind")
        {
            if (state == "behind" && String.Equals(plan.Field("dirty"), "true", StringComparison.OrdinalIgnoreCase))
                throw new InvalidOperationException("工作区有未提交的修改，请先提交或储藏后再快进。");
            detail += state == "ahead" ? "推送本地领先的 " + plan.Field("ahead") + " 个提交到远端？" :
                                         "从远端快进 " + plan.Field("behind") + " 个提交，并更新工作区？";
            if (MessageBox.Show(this, detail, "同步远端 Git", MessageBoxButtons.YesNo, MessageBoxIcon.Question,
                                MessageBoxDefaultButton.Button2) != DialogResult.Yes) return;
            args.AddRange(new[] { "--apply", "--expected-head", plan.Field("head"), "--expected-upstream",
                                  plan.Field("upstreamHead"), "--plan-token", plan.Field("planToken") });
            var done = await Bridge.Call(null, args.ToArray());
            shell.ApplyGitState(project.Field("unitId"), done);
            status.Text = "Git 同步已完成";
        }
        else
        {
            var description = state == "equal" ? "本地分支与远端一致。" :
                state == "diverged" ? "分支已分叉。请在 Git 中合并或变基后再同步。" :
                "请检查本机上游设置。Git 状态为 " + state;
            MessageBox.Show(this, detail + description, "同步远端 Git");
        }
        await shell.RefreshProjects(false);
        RenderProjects();
    }

    async Task RemoveLibrary(string operation, string id)
    {
        var read = new List<string> { "library", operation + "-plan" };
        if (id != null) read.Add(id);
        var plan = await Bridge.Call(null, read.ToArray());
        string name = plan.Field("name"), token = plan.Field("token");
        if (String.IsNullOrEmpty(token)) throw new InvalidOperationException("请重新预览操作。");
        string title = operation == "delete" ? "从 NAS 删除资料库" :
                       operation == "forget" ? "移除本机记录" : "清除本机同步数据";
        var args = new List<string> { "library", operation };
        if (id != null) args.Add(id);
        args.AddRange(new[] { "--token", token });
        if (operation == "delete")
        {
            if (name.Length == 0) throw new InvalidOperationException("请重新读取资料库名称。");
            string detail = "所有获授权设备都会失去这些 NAS 版本。\n资料库与项目 " + plan.Field("units") +
                " 个，版本 " + plan.Field("snapshots") + " 个，受影响设备 " + plan.Field("devices") + " 台。\n" +
                "移除版本保留记录 " + plan.Field("holds") + " 项，取消进行中的上传 " + plan.Field("uploads") + " 项。\n请输入完整名称确认删除\n" + name;
            using (var f = new Form { Text = title, StartPosition = FormStartPosition.CenterParent,
                                      ClientSize = new Size(466, 230), MinimizeBox = false, MaximizeBox = false,
                                      FormBorderStyle = FormBorderStyle.FixedDialog })
            {
                var layout = Column(); layout.Padding = new Padding(16);
                Row(layout, Occupy(Style.Text(detail, 10, false, Style.Ink)), -1);
                var typed = new TextBox { Dock = DockStyle.Fill, AccessibleName = "资料库名称" };
                Row(layout, typed, 32);
                var bar = Bar();
                var cancel = Style.Action("取消", delegate { f.DialogResult = DialogResult.Cancel; });
                var remove = Style.Action("从 NAS 删除", delegate { f.DialogResult = DialogResult.OK; });
                remove.Enabled = false;
                typed.TextChanged += delegate { remove.Enabled = String.Equals(typed.Text,name,StringComparison.Ordinal); };
                bar.Controls.Add(cancel); bar.Controls.Add(remove); Row(layout, bar, 36);
                f.Controls.Add(layout); f.CancelButton = cancel;
                if (f.ShowDialog(this) != DialogResult.OK || typed.Text != name) return;
            }
            args.AddRange(new[] { "--name", name });
        }
        else
        {
            string scope = operation == "forget" ? name + "\n" + plan.Field("path") :
                           "这台电脑的全部同步记录，共 " + plan.Field("bindings") + " 个绑定";
            string message = scope + "\n将移除 " + plan.Field("snapshots") + " 个本机快照和 " +
                plan.Field("queued") + " 条待上传记录。\n缓存记录 " + plan.Field("cached") + " 条（含仅 NAS " +
                plan.Field("nasOnly") + " 条）。工作区原文件和 NAS 备份会保留。";
            if (operation == "reset") message += "\n将移除本机配对凭据，再次使用需要重新配对。";
            if (MessageBox.Show(this,message,title,MessageBoxButtons.OKCancel,MessageBoxIcon.Warning,
                                MessageBoxDefaultButton.Button2) != DialogResult.OK) return;
        }
        while (true)
        {
            try { await Bridge.Call(null,args.ToArray()); break; }
            catch (InvalidOperationException ex)
            {
                bool retryable = ex.Message.Contains("使用原确认码重试") || ex.Message.Contains("此项目正在使用") ||
                                 ex.Message.Contains("此项目正在清理") || ex.Message.Contains("本机正在执行同步或清理") || ex.Message.Contains("旧版维护锁");
                if (!retryable || MessageBox.Show(this, ex.Message, title,
                    MessageBoxButtons.RetryCancel, MessageBoxIcon.Warning) != DialogResult.Retry) throw;
            }
        }
        await shell.RefreshProjects(false);
        await RefreshPage();
        status.Text = title + "已完成";
    }
}
}
